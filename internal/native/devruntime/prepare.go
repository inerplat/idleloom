package devruntime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	receiptVersion = 2
	venvMarkerV3   = "sealed-v3-no-bytecode"
)

type Layout struct {
	Root       string
	Wheelhouse string
	Venv       string
	Model      string
	Work       string
	Runner     string
	Receipt    string
}

type Receipt struct {
	Version           int         `json:"version"`
	DevelopmentOnly   bool        `json:"developmentOnly"`
	ArtifactIdentity  string      `json:"artifactIdentity"`
	ManifestDigest    string      `json:"manifestDigest"`
	RuntimeLockDigest string      `json:"runtimeLockDigest"`
	ModelRepository   string      `json:"modelRepository"`
	ModelRevision     string      `json:"modelRevision"`
	RuntimeVersion    string      `json:"runtimeVersion"`
	RunnerDigest      string      `json:"runnerDigest"`
	ModelFiles        []ModelFile `json:"modelFiles"`
}

type Preparer struct {
	Root       string
	Python     string
	HTTPClient *http.Client
	Progress   func(string)
}

func DefaultRoot() string {
	return filepath.Join(string(filepath.Separator), "var", "tmp", "idleloom")
}

func NewLayout(root string) Layout {
	return Layout{
		Root:       root,
		Wheelhouse: filepath.Join(root, "wheelhouse"),
		Venv:       filepath.Join(root, "runtime", "venv"),
		Model:      filepath.Join(root, "models", "qwen3.5-0.8b-4bit"),
		Work:       filepath.Join(root, "work"),
		Runner:     filepath.Join(root, "runtime", "runner.py"),
		Receipt:    filepath.Join(root, "receipt.json"),
	}
}

func (p Preparer) Prepare(ctx context.Context) (Receipt, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return Receipt{}, fmt.Errorf("native development runtime requires macOS on Apple Silicon")
	}
	root := p.Root
	if root == "" {
		root = DefaultRoot()
	}
	python := p.Python
	if python == "" {
		python = "python3"
	}
	layout := NewLayout(root)
	for _, dir := range []string{layout.Root, layout.Wheelhouse, filepath.Dir(layout.Venv), layout.Model, layout.Work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Receipt{}, fmt.Errorf("create development runtime directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Receipt{}, fmt.Errorf("restrict development runtime directory: %w", err)
		}
	}

	runtimeFiles, runtimeDigest, err := RuntimeLock()
	if err != nil {
		return Receipt{}, err
	}
	modelFiles, modelDigest, err := ModelLock()
	if err != nil {
		return Receipt{}, err
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	for _, file := range runtimeFiles {
		p.progress("runtime " + file.Package + "==" + file.Version)
		if err := ensureDownload(ctx, client, file.URL, filepath.Join(layout.Wheelhouse, file.Name), 1<<30, file.SHA256, 0); err != nil {
			return Receipt{}, err
		}
	}
	for _, file := range modelFiles {
		p.progress("model " + file.Path)
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s?download=true", ModelRepository, ModelRevision, file.Path)
		if err := ensureDownload(ctx, client, url, filepath.Join(layout.Model, file.Path), file.Size, file.SHA256, file.Size); err != nil {
			return Receipt{}, err
		}
	}

	if err := p.prepareVenv(ctx, python, layout, runtimeDigest); err != nil {
		return Receipt{}, err
	}
	runner, err := RunnerSource()
	if err != nil {
		return Receipt{}, err
	}
	if err := atomicWrite(layout.Runner, runner, 0o500); err != nil {
		return Receipt{}, fmt.Errorf("write embedded runner: %w", err)
	}
	receipt := Receipt{
		Version:           receiptVersion,
		DevelopmentOnly:   true,
		ArtifactIdentity:  "oci://development.invalid/idleloom/qwen3.5-0.8b-4bit@sha256:" + modelDigest,
		ManifestDigest:    "sha256:" + modelDigest,
		RuntimeLockDigest: "sha256:" + runtimeDigest,
		ModelRepository:   ModelRepository,
		ModelRevision:     ModelRevision,
		RuntimeVersion:    RuntimeVersion,
		RunnerDigest:      "sha256:" + digest(runner),
		ModelFiles:        modelFiles,
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return Receipt{}, err
	}
	data = append(data, '\n')
	if err := atomicWrite(layout.Receipt, data, 0o600); err != nil {
		return Receipt{}, fmt.Errorf("write development receipt: %w", err)
	}
	return receipt, nil
}

func (p Preparer) prepareVenv(ctx context.Context, python string, layout Layout, runtimeDigest string) error {
	marker := filepath.Join(layout.Venv, ".idleloom-runtime-lock")
	expectedMarker := runtimeDigest + ":" + venvMarkerV3
	if data, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(data)) == expectedMarker {
		return nil
	}
	tmp := layout.Venv + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, python, "-m", "venv", tmp)
	cmd.Env = offlineEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create Python environment: %w: %s", err, strings.TrimSpace(string(output)))
	}
	pip := filepath.Join(tmp, "bin", "python")
	cmd = exec.CommandContext(ctx, pip, "-I", "-m", "pip", "install", "--disable-pip-version-check", "--no-compile", "--no-index", "--find-links", layout.Wheelhouse, "mlx-lm==0.31.3")
	cmd.Env = offlineEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install locked Python environment: %w: %s", err, strings.TrimSpace(string(output)))
	}
	cmd = exec.CommandContext(ctx, pip, "-I", "-m", "pip", "uninstall", "-y", "pip")
	cmd.Env = offlineEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove installer from locked Python environment: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := removeBytecodeCaches(tmp); err != nil {
		return fmt.Errorf("remove Python bytecode caches: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".idleloom-runtime-lock"), []byte(expectedMarker+"\n"), 0o400); err != nil {
		return err
	}
	if err := os.RemoveAll(layout.Venv); err != nil {
		return err
	}
	if err := os.Rename(tmp, layout.Venv); err != nil {
		return fmt.Errorf("activate Python environment: %w", err)
	}
	return nil
}

func Verify(layout Layout) (Receipt, error) {
	return verify(layout, true)
}

// VerifyFast checks the sealed receipt, runner, package inventory, and model
// file shape without rereading multi-gigabyte model contents. Process startup
// must still call Verify before executing the model.
func VerifyFast(layout Layout) (Receipt, error) {
	return verify(layout, false)
}

func verify(layout Layout, strong bool) (Receipt, error) {
	data, err := os.ReadFile(layout.Receipt)
	if err != nil {
		return Receipt{}, fmt.Errorf("read development receipt: %w", err)
	}
	var receipt Receipt
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, fmt.Errorf("decode development receipt: %w", err)
	}
	runtimeFiles, runtimeDigest, err := RuntimeLock()
	if err != nil {
		return Receipt{}, err
	}
	files, modelDigest, err := ModelLock()
	if err != nil {
		return Receipt{}, err
	}
	runner, err := RunnerSource()
	if err != nil {
		return Receipt{}, err
	}
	if !receiptMatchesBinary(receipt, runtimeDigest, modelDigest, files, runner) {
		return Receipt{}, fmt.Errorf("development receipt does not match this binary")
	}
	if err := verifyFile(layout.Runner, digest(runner), int64(len(runner))); err != nil {
		return Receipt{}, fmt.Errorf("verify embedded runner: %w", err)
	}
	marker, err := os.ReadFile(filepath.Join(layout.Venv, ".idleloom-runtime-lock"))
	if err != nil || strings.TrimSpace(string(marker)) != runtimeDigest+":"+venvMarkerV3 {
		return Receipt{}, fmt.Errorf("Python environment lock marker does not match this binary")
	}
	if err := verifyInstalledPackages(layout, runtimeFiles); err != nil {
		return Receipt{}, err
	}
	if strong {
		if err := verifyInstalledRuntime(layout, runtimeFiles); err != nil {
			return Receipt{}, err
		}
	}
	for _, file := range files {
		path := filepath.Join(layout.Model, file.Path)
		var err error
		if strong {
			err = verifyFile(path, file.SHA256, file.Size)
		} else {
			err = verifyRegularFile(path, file.Size)
		}
		if err != nil {
			return Receipt{}, err
		}
	}
	if _, err := os.Stat(filepath.Join(layout.Venv, "bin", "python")); err != nil {
		return Receipt{}, fmt.Errorf("verify Python environment: %w", err)
	}
	return receipt, nil
}

func receiptMatchesBinary(receipt Receipt, runtimeDigest, modelDigest string, files []ModelFile, runner []byte) bool {
	expectedArtifact := "oci://development.invalid/idleloom/qwen3.5-0.8b-4bit@sha256:" + modelDigest
	return receipt.Version == receiptVersion && receipt.DevelopmentOnly &&
		receipt.ArtifactIdentity == expectedArtifact && receipt.ManifestDigest == "sha256:"+modelDigest &&
		receipt.RuntimeLockDigest == "sha256:"+runtimeDigest && receipt.ModelRepository == ModelRepository &&
		receipt.ModelRevision == ModelRevision && receipt.RuntimeVersion == RuntimeVersion &&
		receipt.RunnerDigest == "sha256:"+digest(runner) && reflect.DeepEqual(receipt.ModelFiles, files)
}

func verifyRegularFile(name string, expectedSize int64) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return fmt.Errorf("%s does not match the locked regular file shape", filepath.Base(name))
	}
	return nil
}

func verifyInstalledPackages(layout Layout, expected []RuntimeFile) error {
	directory := filepath.Join(layout.Venv, "lib", "python3.12", "site-packages")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read installed Python packages: %w", err)
	}
	actual := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dist-info") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name(), "METADATA"))
		if err != nil || len(data) > 1<<20 {
			return fmt.Errorf("read installed package metadata %s", entry.Name())
		}
		var name, version string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Name: ") {
				name = normalizePackage(strings.TrimSpace(strings.TrimPrefix(line, "Name: ")))
			}
			if strings.HasPrefix(line, "Version: ") {
				version = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
			}
			if name != "" && version != "" {
				break
			}
		}
		if name == "" || version == "" {
			return fmt.Errorf("installed package metadata %s is incomplete", entry.Name())
		}
		actual[name] = version
	}
	want := make(map[string]string, len(expected))
	for _, file := range expected {
		want[normalizePackage(file.Package)] = file.Version
	}
	if !reflect.DeepEqual(actual, want) {
		return fmt.Errorf("installed Python package inventory does not match runtime lock: got %s, want %s", formatInventory(actual), formatInventory(want))
	}
	return nil
}

func verifyInstalledRuntime(layout Layout, expected []RuntimeFile) error {
	sitePackages := filepath.Join(layout.Venv, "lib", "python3.12", "site-packages")
	allowed := make(map[string]struct{})
	for _, locked := range expected {
		wheelPath := filepath.Join(layout.Wheelhouse, locked.Name)
		if err := verifyDigestOnly(wheelPath, locked.SHA256); err != nil {
			return fmt.Errorf("verify locked wheel %s: %w", locked.Name, err)
		}
		wheel, err := zip.OpenReader(wheelPath)
		if err != nil {
			return fmt.Errorf("open locked wheel %s: %w", locked.Name, err)
		}
		err = verifyWheelInstallation(sitePackages, wheel.File, allowed)
		wheel.Close()
		if err != nil {
			return fmt.Errorf("verify installed wheel %s: %w", locked.Name, err)
		}
	}
	return filepath.WalkDir(sitePackages, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(sitePackages, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, ok := allowed[relative]; ok {
			return nil
		}
		if generatedRuntimeFile(relative) {
			return nil
		}
		return fmt.Errorf("unexpected Python runtime file %s", relative)
	})
}

func verifyWheelInstallation(sitePackages string, files []*zip.File, allowed map[string]struct{}) error {
	var record *zip.File
	for _, file := range files {
		if strings.HasSuffix(file.Name, ".dist-info/RECORD") {
			if record != nil {
				return fmt.Errorf("wheel contains multiple RECORD files")
			}
			record = file
		}
	}
	if record == nil {
		return fmt.Errorf("wheel has no RECORD file")
	}
	reader, err := record.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	rows := csv.NewReader(io.LimitReader(reader, 16<<20))
	for {
		row, err := rows.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode wheel RECORD: %w", err)
		}
		if len(row) != 3 {
			return fmt.Errorf("wheel RECORD row has %d fields, want 3", len(row))
		}
		if row[1] == "" {
			continue
		}
		if !strings.HasPrefix(row[1], "sha256=") {
			return fmt.Errorf("unsupported wheel RECORD hash for %s", row[0])
		}
		digestBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(row[1], "sha256="))
		if err != nil || len(digestBytes) != sha256.Size {
			return fmt.Errorf("invalid wheel RECORD hash for %s", row[0])
		}
		size, err := strconv.ParseInt(row[2], 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("invalid wheel RECORD size for %s", row[0])
		}
		relative, installed, err := installedWheelPath(row[0])
		if err != nil {
			return err
		}
		if !installed {
			continue
		}
		if err := verifyFile(filepath.Join(sitePackages, filepath.FromSlash(relative)), hex.EncodeToString(digestBytes), size); err != nil {
			return fmt.Errorf("%s: %w", relative, err)
		}
		allowed[relative] = struct{}{}
	}
	return nil
}

func installedWheelPath(name string) (string, bool, error) {
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) {
		return "", false, fmt.Errorf("invalid wheel RECORD path %q", name)
	}
	if strings.HasPrefix(clean, "../") {
		return "", false, nil
	}
	if marker := ".data/purelib/"; strings.Contains(clean, marker) {
		clean = strings.SplitN(clean, marker, 2)[1]
	} else if marker := ".data/platlib/"; strings.Contains(clean, marker) {
		clean = strings.SplitN(clean, marker, 2)[1]
	} else if strings.Contains(clean, ".data/") {
		return "", false, nil
	}
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", false, fmt.Errorf("invalid installed wheel path %q", name)
	}
	return clean, true, nil
}

func generatedRuntimeFile(relative string) bool {
	for _, suffix := range []string{"/INSTALLER", "/REQUESTED", "/RECORD", "/direct_url.json"} {
		if strings.HasSuffix(relative, ".dist-info"+suffix) {
			return true
		}
	}
	return false
}

func removeBytecodeCaches(root string) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == "__pycache__" {
			if err := os.RemoveAll(name); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".pyc") {
			return os.Remove(name)
		}
		return nil
	})
}

func verifyDigestOnly(name, expectedDigest string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(name))
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expectedDigest {
		return fmt.Errorf("%s SHA-256 is %s, want %s", filepath.Base(name), actual, expectedDigest)
	}
	return nil
}

func normalizePackage(value string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(value))
}

func formatInventory(values map[string]string) string {
	items := make([]string, 0, len(values))
	for name, version := range values {
		items = append(items, name+"=="+version)
	}
	sort.Strings(items)
	return strings.Join(items, ",")
}

func ensureDownload(ctx context.Context, client *http.Client, url, destination string, maxBytes int64, expectedDigest string, expectedSize int64) error {
	if err := verifyFile(destination, expectedDigest, expectedSize); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", filepath.Base(destination), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected HTTP status %s", filepath.Base(destination), response.Status)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", filepath.Base(destination), err)
	}
	if written > maxBytes || (expectedSize > 0 && written != expectedSize) {
		return fmt.Errorf("download %s: unexpected size %d", filepath.Base(destination), written)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != expectedDigest {
		return fmt.Errorf("download %s: SHA-256 mismatch", filepath.Base(destination))
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return err
	}
	return nil
}

func verifyFile(name, expectedDigest string, expectedSize int64) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (expectedSize > 0 && info.Size() != expectedSize) {
		return fmt.Errorf("%s does not match the locked regular file", filepath.Base(name))
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("%s SHA-256 mismatch", filepath.Base(name))
	}
	return nil
}

func atomicWrite(name string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, name); err != nil {
		return err
	}
	return nil
}

func offlineEnv() []string {
	return []string{
		"HOME=/var/empty",
		"LANG=C.UTF-8",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"PIP_CONFIG_FILE=/dev/null",
		"PIP_DISABLE_PIP_VERSION_CHECK=1",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONNOUSERSITE=1",
	}
}

func (p Preparer) progress(message string) {
	if p.Progress != nil {
		p.Progress(message)
	}
}

var _ = errors.Is
