package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTrainingOutputProtocolKeepsBoundedLatestSummaries(t *testing.T) {
	agent := &DevAgent{runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"}}
	now := time.Unix(1_800_000_000, 0).UTC()
	agent.observeRunProtocol(now, metricProtocolPrefix+`{"name":"loss","value":1.5,"step":1}`)
	agent.observeRunProtocol(now.Add(time.Second), metricProtocolPrefix+`{"name":"loss","value":0.5,"step":2}`)
	agent.observeRunProtocol(now, artifactProtocolPrefix+`{"name":"checkpoint","uri":"s3://models/run/checkpoint.npz","digest":"sha256:`+strings.Repeat("a", 64)+`","sizeBytes":42}`)
	if agent.runProtocolErr != nil {
		t.Fatal(agent.runProtocolErr)
	}
	if len(agent.runStatus.Metrics) != 1 || agent.runStatus.Metrics[0].Value != "0.5" || agent.runStatus.Metrics[0].Step != 2 {
		t.Fatalf("metrics = %#v", agent.runStatus.Metrics)
	}
	if len(agent.runStatus.Artifacts) != 1 || agent.runStatus.Artifacts[0].Name != "checkpoint" {
		t.Fatalf("artifacts = %#v", agent.runStatus.Artifacts)
	}
}

func TestTrainingOutputProtocolVerifiesLocalArtifact(t *testing.T) {
	layout := devruntime.NewLayout(t.TempDir())
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment")},
		Spec:       nativev1alpha1.IdleloomWorkloadAssignmentSpec{Training: &nativev1alpha1.ResolvedTraining{}},
	}
	work := shellWorkDirectory(layout, assignment.UID)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("checkpoint")
	path := filepath.Join(work, "checkpoint.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	agent := &DevAgent{
		config: DevAgentConfig{Layout: layout}, assignment: assignment,
		runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"},
	}
	payload := artifactProtocolPrefix + `{"name":"checkpoint","uri":` + quoteJSON((&url.URL{Scheme: "file", Path: canonicalPath}).String()) + `,"digest":"sha256:` + hex.EncodeToString(digest[:]) + `"}`
	agent.observeRunProtocol(time.Now(), payload)
	if agent.runProtocolErr != nil {
		t.Fatal(agent.runProtocolErr)
	}
	if len(agent.runStatus.Artifacts) != 1 || agent.runStatus.Artifacts[0].SizeBytes != int64(len(data)) {
		t.Fatalf("artifacts = %#v", agent.runStatus.Artifacts)
	}
}

func TestTrainingOutputProtocolRejectsLocalArtifactSymlinkEscape(t *testing.T) {
	layout := devruntime.NewLayout(t.TempDir())
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment")},
		Spec:       nativev1alpha1.IdleloomWorkloadAssignmentSpec{Training: &nativev1alpha1.ResolvedTraining{}},
	}
	work := shellWorkDirectory(layout, assignment.UID)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("outside")
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(work, "checkpoint.bin")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	agent := &DevAgent{
		config: DevAgentConfig{Layout: layout}, assignment: assignment,
		runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"},
	}
	payload := artifactProtocolPrefix + `{"name":"checkpoint","uri":` + quoteJSON((&url.URL{Scheme: "file", Path: link}).String()) + `,"digest":"sha256:` + hex.EncodeToString(digest[:]) + `"}`
	agent.observeRunProtocol(time.Now(), payload)
	if agent.runProtocolErr == nil {
		t.Fatal("artifact symlink outside the work directory was accepted")
	}
}

func TestStableArtifactHashRejectsConcurrentSizeChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "append",
			mutate: func(t *testing.T, path string) {
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("-appended"); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "truncate",
			mutate: func(t *testing.T, path string) {
				if err := os.Truncate(path, 3); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checkpoint.bin")
			if err := os.WriteFile(path, []byte("checkpoint"), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			initial, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path)
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(current)
			if _, err := verifyStableArtifactFile(file, initial, "sha256:"+hex.EncodeToString(digest[:])); err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("mutated artifact error = %v", err)
			}
		})
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestTrainingOutputProtocolRejectsMalformedRecords(t *testing.T) {
	agent := &DevAgent{runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"}}
	agent.observeRunProtocol(time.Now(), metricProtocolPrefix+`{"name":"loss","value":"secret"}`)
	if agent.runProtocolErr == nil {
		t.Fatal("malformed metric was accepted")
	}
}

func TestTrainingOutputProtocolRejectsOversizedRecords(t *testing.T) {
	agent := &DevAgent{runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"}}
	agent.observeRunProtocol(time.Now(), metricProtocolPrefix+`{"name":"loss","value":1,"padding":"`+strings.Repeat("x", maxMetricRecordBytes)+`"}`)
	if agent.runProtocolErr == nil || !strings.Contains(agent.runProtocolErr.Error(), "exceeds") {
		t.Fatalf("oversized metric error = %v", agent.runProtocolErr)
	}

	agent = &DevAgent{runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"}}
	agent.observeRunProtocol(time.Now(), artifactProtocolPrefix+`{"name":"checkpoint","uri":"s3://bucket/`+strings.Repeat("x", maxArtifactURIBytes)+`","digest":"sha256:`+strings.Repeat("a", 64)+`"}`)
	if agent.runProtocolErr == nil || !strings.Contains(agent.runProtocolErr.Error(), "exceeds") {
		t.Fatalf("oversized artifact error = %v", agent.runProtocolErr)
	}
}

func TestTrainingOutputProtocolRejectsArtifactQueryCredentials(t *testing.T) {
	agent := &DevAgent{runStatus: &nativev1alpha1.WorkloadRunStatus{ID: "run"}}
	agent.observeRunProtocol(time.Now(), artifactProtocolPrefix+`{"name":"checkpoint","uri":"https://objects.example/checkpoint?token=secret","digest":"sha256:`+strings.Repeat("a", 64)+`"}`)
	if agent.runProtocolErr == nil {
		t.Fatal("artifact URI query was accepted")
	}
}
