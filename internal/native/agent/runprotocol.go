package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	metricProtocolPrefix   = "::idleloom-metric::"
	artifactProtocolPrefix = "::idleloom-artifact::"
	maxRunMetrics          = 32
	maxRunArtifacts        = 16
	maxMetricRecordBytes   = 4096
	maxArtifactRecordBytes = 8192
	maxMetricValueBytes    = 128
	maxArtifactURIBytes    = 4096
)

type metricProtocolRecord struct {
	Name  string      `json:"name"`
	Value json.Number `json:"value"`
	Step  int64       `json:"step,omitempty"`
}

type artifactProtocolRecord struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

func (a *DevAgent) observeRunProtocol(now time.Time, line string) {
	var err error
	switch {
	case strings.HasPrefix(line, metricProtocolPrefix):
		err = a.observeMetric(now, strings.TrimPrefix(line, metricProtocolPrefix))
	case strings.HasPrefix(line, artifactProtocolPrefix):
		err = a.observeArtifact(strings.TrimPrefix(line, artifactProtocolPrefix))
	default:
		return
	}
	if err == nil {
		return
	}
	a.mu.Lock()
	if a.runProtocolErr == nil {
		a.runProtocolErr = err
	}
	a.mu.Unlock()
	a.appendLog(now, "invalid training output protocol: %v", err)
}

func (a *DevAgent) observeMetric(now time.Time, payload string) error {
	if len([]byte(payload)) > maxMetricRecordBytes {
		return fmt.Errorf("metric record exceeds %d bytes", maxMetricRecordBytes)
	}
	var record metricProtocolRecord
	if err := decodeProtocolRecord(payload, &record); err != nil {
		return fmt.Errorf("metric: %w", err)
	}
	if problems := validation.IsDNS1123Label(record.Name); len(problems) > 0 {
		return fmt.Errorf("metric name %q is invalid: %s", record.Name, strings.Join(problems, "; "))
	}
	value, err := record.Value.Float64()
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("metric %s value must be finite", record.Name)
	}
	if record.Step < 0 {
		return fmt.Errorf("metric %s step must not be negative", record.Name)
	}
	if len(record.Value.String()) > maxMetricValueBytes {
		return fmt.Errorf("metric %s value exceeds %d bytes", record.Name, maxMetricValueBytes)
	}
	metric := nativev1alpha1.RunMetricSummary{
		Name: record.Name, Value: record.Value.String(), Step: record.Step, ObservedAt: metav1.NewMicroTime(now),
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runStatus == nil {
		return fmt.Errorf("metric was emitted outside a tracked run")
	}
	for index := range a.runStatus.Metrics {
		if a.runStatus.Metrics[index].Name == record.Name {
			a.runStatus.Metrics[index] = metric
			return nil
		}
	}
	if len(a.runStatus.Metrics) >= maxRunMetrics {
		return fmt.Errorf("run emitted more than %d distinct metrics", maxRunMetrics)
	}
	a.runStatus.Metrics = append(a.runStatus.Metrics, metric)
	sort.Slice(a.runStatus.Metrics, func(i, j int) bool { return a.runStatus.Metrics[i].Name < a.runStatus.Metrics[j].Name })
	return nil
}

func (a *DevAgent) observeArtifact(payload string) error {
	if len([]byte(payload)) > maxArtifactRecordBytes {
		return fmt.Errorf("artifact record exceeds %d bytes", maxArtifactRecordBytes)
	}
	var record artifactProtocolRecord
	if err := decodeProtocolRecord(payload, &record); err != nil {
		return fmt.Errorf("artifact: %w", err)
	}
	if problems := validation.IsDNS1123Label(record.Name); len(problems) > 0 {
		return fmt.Errorf("artifact name %q is invalid: %s", record.Name, strings.Join(problems, "; "))
	}
	parsed, err := url.Parse(record.URI)
	if err != nil || parsed.Scheme == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("artifact %s URI must be absolute and must not contain credentials, a query, or a fragment", record.Name)
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
		return fmt.Errorf("artifact %s HTTP URI has no host", record.Name)
	}
	if len([]byte(record.URI)) > maxArtifactURIBytes {
		return fmt.Errorf("artifact %s URI exceeds %d bytes", record.Name, maxArtifactURIBytes)
	}
	if len(record.Digest) != len("sha256:")+64 || !strings.HasPrefix(record.Digest, "sha256:") {
		return fmt.Errorf("artifact %s digest must be sha256", record.Name)
	}
	for _, character := range strings.TrimPrefix(record.Digest, "sha256:") {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return fmt.Errorf("artifact %s digest must be lowercase hexadecimal", record.Name)
			}
		}
	}
	if record.SizeBytes < 0 {
		return fmt.Errorf("artifact %s size must not be negative", record.Name)
	}
	if parsed.Scheme == "file" {
		if parsed.Host != "" {
			return fmt.Errorf("artifact %s file URI must not contain a host", record.Name)
		}
		verifiedSize, err := a.verifyLocalArtifact(parsed.Path, record.Digest, record.SizeBytes)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", record.Name, err)
		}
		record.SizeBytes = verifiedSize
	}
	artifact := nativev1alpha1.RunArtifactReference{
		Name: record.Name, URI: record.URI, Digest: record.Digest, SizeBytes: record.SizeBytes,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runStatus == nil {
		return fmt.Errorf("artifact was emitted outside a tracked run")
	}
	for index := range a.runStatus.Artifacts {
		if a.runStatus.Artifacts[index].Name == record.Name {
			a.runStatus.Artifacts[index] = artifact
			return nil
		}
	}
	if len(a.runStatus.Artifacts) >= maxRunArtifacts {
		return fmt.Errorf("run emitted more than %d artifacts", maxRunArtifacts)
	}
	a.runStatus.Artifacts = append(a.runStatus.Artifacts, artifact)
	sort.Slice(a.runStatus.Artifacts, func(i, j int) bool { return a.runStatus.Artifacts[i].Name < a.runStatus.Artifacts[j].Name })
	return nil
}

func (a *DevAgent) verifyLocalArtifact(path, digest string, declaredSize int64) (int64, error) {
	a.mu.RLock()
	assignment := a.assignment
	layout := a.config.Layout
	a.mu.RUnlock()
	if assignment == nil || assignment.Spec.Training == nil {
		return 0, fmt.Errorf("local artifacts are accepted only from an active training run")
	}
	work := shellWorkDirectory(layout, assignment.UID)
	canonicalWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		return 0, fmt.Errorf("resolve training work directory: %w", err)
	}
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("local artifact path must be absolute")
	}
	relativePath, inside := relativePathWithin(filepath.Clean(work), filepath.Clean(path))
	if !inside {
		relativePath, inside = relativePathWithin(canonicalWork, filepath.Clean(path))
	}
	if !inside {
		return 0, fmt.Errorf("local artifact is outside the training work directory")
	}
	root, err := os.OpenRoot(canonicalWork)
	if err != nil {
		return 0, fmt.Errorf("open training work directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(relativePath)
	if err != nil {
		return 0, fmt.Errorf("open local artifact within the training work directory: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 64<<30 {
		return 0, fmt.Errorf("local artifact must be a regular file no larger than 64 GiB")
	}
	if declaredSize != 0 && declaredSize != info.Size() {
		return 0, fmt.Errorf("declared size %d does not match file size %d", declaredSize, info.Size())
	}
	return verifyStableArtifactFile(file, info, digest)
}

func verifyStableArtifactFile(file *os.File, initial os.FileInfo, digest string) (int64, error) {
	if file == nil || initial == nil {
		return 0, fmt.Errorf("local artifact file and initial metadata are required")
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, initial.Size()+1))
	if err != nil {
		return 0, err
	}
	if read != initial.Size() {
		return 0, fmt.Errorf("local artifact changed while it was being hashed: read %d bytes from an initial size of %d", read, initial.Size())
	}
	after, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if !os.SameFile(initial, after) || after.Size() != initial.Size() || after.Mode() != initial.Mode() || !after.ModTime().Equal(initial.ModTime()) {
		return 0, fmt.Errorf("local artifact changed while it was being hashed")
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		return 0, fmt.Errorf("digest %s does not match %s", digest, actual)
	}
	return initial.Size(), nil
}

func relativePathWithin(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

func decodeProtocolRecord(payload string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("record contains trailing data")
	}
	return nil
}

func cloneRunStatus(status *nativev1alpha1.WorkloadRunStatus) *nativev1alpha1.WorkloadRunStatus {
	if status == nil {
		return nil
	}
	copy := *status
	copy.Metrics = append([]nativev1alpha1.RunMetricSummary(nil), status.Metrics...)
	copy.Artifacts = append([]nativev1alpha1.RunArtifactReference(nil), status.Artifacts...)
	if status.StartedAt != nil {
		value := *status.StartedAt
		copy.StartedAt = &value
	}
	if status.FinishedAt != nil {
		value := *status.FinishedAt
		copy.FinishedAt = &value
	}
	return &copy
}
