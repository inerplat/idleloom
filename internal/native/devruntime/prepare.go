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
	"sync"
	"time"
)

const (
	receiptVersion        = 2
	runtimeReceiptVersion = 1
	venvMarkerV3          = "sealed-v3-no-bytecode"
)

type Layout struct {
	Root           string
	Wheelhouse     string
	Venv           string
	Model          string
	Work           string
	Runner         string
	Receipt        string
	RuntimeReceipt string
}

type RuntimeReceipt struct {
	Version           int    `json:"version"`
	RuntimeLockDigest string `json:"runtimeLockDigest"`
	RuntimeVersion    string `json:"runtimeVersion"`
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

var mlxPlatformCheck struct {
	once sync.Once
	err  error
}

func DefaultRoot() string {
	return filepath.Join(string(filepath.Separator), "var", "tmp", "idleloom")
}

func NewLayout(root string) Layout {
	return Layout{
		Root:           root,
		Wheelhouse:     filepath.Join(root, "wheelhouse"),
		Venv:           filepath.Join(root, "runtime", "venv"),
		Model:          filepath.Join(root, "models", "qwen3.5-0.8b-4bit"),
		Work:           filepath.Join(root, "work"),
		Runner:         filepath.Join(root, "runtime", "runner.py"),
		Receipt:        filepath.Join(root, "receipt.json"),
		RuntimeReceipt: filepath.Join(root, "runtime", "receipt.json"),
	}
}

func (p Preparer) Prepare(ctx context.Context) (Receipt, error) {
	runtimeReceipt, err := p.PrepareRuntime(ctx)
	if err != nil {
		return Receipt{}, err
	}
	layout := NewLayout(p.root())
	modelFiles, modelDigest, err := ModelLock()
	if err != nil {
		return Receipt{}, err
	}
	client := p.client()
	if err := os.MkdirAll(layout.Model, 0o700); err != nil {
		return Receipt{}, fmt.Errorf("create development model directory: %w", err)
	}
	if err := os.Chmod(layout.Model, 0o700); err != nil {
		return Receipt{}, fmt.Errorf("restrict development model directory: %w", err)
	}
	for _, file := range modelFiles {
		p.progress("model " + file.Path)
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s?download=true", ModelRepository, ModelRevision, file.Path)
		if err := ensureDownload(ctx, client, url, filepath.Join(layout.Model, file.Path), file.Size, file.SHA256, file.Size); err != nil {
			return Receipt{}, err
		}
	}
	runner, err := RunnerSource()
	if err != nil {
		return Receipt{}, err
	}
	if err := atomicWrite(layout.Runner, runner, 0o500); err != nil {
		return Receipt{}, fmt.Errorf("write embedded runner: %w", err)
	}
	descriptor := lockedModelDescriptor(modelFiles, modelDigest)
	receipt := Receipt{
		Version:           receiptVersion,
		DevelopmentOnly:   true,
		ArtifactIdentity:  descriptor.ArtifactIdentity,
		ManifestDigest:    descriptor.ManifestDigest,
		RuntimeLockDigest: runtimeReceipt.RuntimeLockDigest,
		ModelRepository:   ModelRepository,
		ModelRevision:     ModelRevision,
		RuntimeVersion:    runtimeReceipt.RuntimeVersion,
		RunnerDigest:      "sha256:" + digest(runner),
		ModelFiles:        modelFiles,
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return Receipt{}, err
	}
	if err := atomicWrite(layout.Receipt, append(data, '\n'), 0o600); err != nil {
		return Receipt{}, fmt.Errorf("write development receipt: %w", err)
	}
	return receipt, nil
}

func (p Preparer) PrepareRuntime(ctx context.Context) (RuntimeReceipt, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return RuntimeReceipt{}, fmt.Errorf("native development runtime requires macOS on Apple Silicon")
	}
	if err := CheckMLXPlatform(); err != nil {
		return RuntimeReceipt{}, err
	}
	root := p.root()
	python, err := FindPython312(p.Python)
	if err != nil {
		return RuntimeReceipt{}, err
	}
	layout := NewLayout(root)
	for _, dir := range []string{layout.Root, layout.Wheelhouse, filepath.Dir(layout.Venv), layout.Work} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return RuntimeReceipt{}, fmt.Errorf("create development runtime directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return RuntimeReceipt{}, fmt.Errorf("restrict development runtime directory: %w", err)
		}
	}

	runtimeFiles, runtimeDigest, err := RuntimeLock()
	if err != nil {
		return RuntimeReceipt{}, err
	}
	client := p.client()
	for _, file := range runtimeFiles {
		p.progress("runtime " + file.Package + "==" + file.Version)
		if err := ensureDownload(ctx, client, file.URL, filepath.Join(layout.Wheelhouse, file.Name), 1<<30, file.SHA256, 0); err != nil {
			return RuntimeReceipt{}, err
		}
	}

	if err := p.prepareVenv(ctx, python, layout, runtimeDigest, runtimeFiles); err != nil {
		return RuntimeReceipt{}, err
	}
	receipt := RuntimeReceipt{
		Version:           runtimeReceiptVersion,
		RuntimeLockDigest: "sha256:" + runtimeDigest,
		RuntimeVersion:    RuntimeVersion,
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return RuntimeReceipt{}, err
	}
	if err := atomicWrite(layout.RuntimeReceipt, append(data, '\n'), 0o600); err != nil {
		return RuntimeReceipt{}, fmt.Errorf("write runtime receipt: %w", err)
	}
	return receipt, nil
}

func CheckMLXPlatform() error {
	mlxPlatformCheck.once.Do(func() {
		mlxPlatformCheck.err = checkMLXPlatform()
	})
	return mlxPlatformCheck.err
}

func checkMLXPlatform() error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("locked MLX runtime requires macOS on Apple Silicon")
	}
	output, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return fmt.Errorf("read macOS product version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if !mlxPlatformVersionSupported(version) {
		return fmt.Errorf("locked MLX 0.32 runtime requires macOS 26 or later; found %q", version)
	}
	return nil
}

func mlxPlatformVersionSupported(version string) bool {
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	return err == nil && major >= 26
}

func FindPython312(explicit string) (string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = []string{"python3.12", "python3"}
	}
	seen := make(map[string]struct{}, len(candidates))
	var detected []string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		command := exec.Command(path, "-I", "-c", "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
		command.Env = offlineEnv()
		output, err := command.Output()
		if err != nil {
			detected = append(detected, filepath.Base(path)+" (unusable)")
			continue
		}
		version := strings.TrimSpace(string(output))
		if version == "3.12" {
			return path, nil
		}
		detected = append(detected, filepath.Base(path)+" "+version)
	}
	if len(detected) > 0 {
		return "", fmt.Errorf("locked MLX runtime requires Python 3.12; found %s", strings.Join(detected, ", "))
	}
	return "", fmt.Errorf("locked MLX runtime requires Python 3.12; install python@3.12 and restart the Native agent")
}

func (p Preparer) root() string {
	if p.Root != "" {
		return p.Root
	}
	return DefaultRoot()
}

func (p Preparer) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

func (p Preparer) prepareVenv(ctx context.Context, python string, layout Layout, runtimeDigest string, runtimeFiles []RuntimeFile) error {
	marker := filepath.Join(layout.Venv, ".idleloom-runtime-lock")
	expectedMarker := runtimeDigest + ":" + venvMarkerV3
	if data, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(data)) == expectedMarker {
		if err := verifyInstalledPackages(layout, runtimeFiles); err == nil {
			if err := verifyInstalledRuntime(layout, runtimeFiles); err == nil {
				return nil
			}
		}
	}
	tmp := layout.Venv + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
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

func VerifyRuntime(layout Layout) (RuntimeReceipt, error) {
	return verifyRuntime(layout, true)
}

func VerifyRuntimeFast(layout Layout) (RuntimeReceipt, error) {
	return verifyRuntime(layout, false)
}

func verify(layout Layout, strong bool) (Receipt, error) {
	if _, err := verifyRuntime(layout, strong); err != nil {
		return Receipt{}, err
	}
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
	_, runtimeDigest, err := RuntimeLock()
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
	return receipt, nil
}

func verifyRuntime(layout Layout, strong bool) (RuntimeReceipt, error) {
	if err := CheckMLXPlatform(); err != nil {
		return RuntimeReceipt{}, err
	}
	data, err := os.ReadFile(layout.RuntimeReceipt)
	if err != nil {
		return RuntimeReceipt{}, fmt.Errorf("read runtime receipt: %w", err)
	}
	var receipt RuntimeReceipt
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return RuntimeReceipt{}, fmt.Errorf("decode runtime receipt: %w", err)
	}
	runtimeFiles, runtimeDigest, err := RuntimeLock()
	if err != nil {
		return RuntimeReceipt{}, err
	}
	if receipt.Version != runtimeReceiptVersion || receipt.RuntimeLockDigest != "sha256:"+runtimeDigest || receipt.RuntimeVersion != RuntimeVersion {
		return RuntimeReceipt{}, fmt.Errorf("runtime receipt does not match this binary")
	}
	marker, err := os.ReadFile(filepath.Join(layout.Venv, ".idleloom-runtime-lock"))
	if err != nil || strings.TrimSpace(string(marker)) != runtimeDigest+":"+venvMarkerV3 {
		return RuntimeReceipt{}, fmt.Errorf("the Python environment lock marker does not match this binary")
	}
	if err := verifyInstalledPackages(layout, runtimeFiles); err != nil {
		return RuntimeReceipt{}, err
	}
	if strong {
		if err := verifyInstalledRuntime(layout, runtimeFiles); err != nil {
			return RuntimeReceipt{}, err
		}
	}
	if _, err := os.Stat(filepath.Join(layout.Venv, "bin", "python")); err != nil {
		return RuntimeReceipt{}, fmt.Errorf("verify Python environment: %w", err)
	}
	return receipt, nil
}

func receiptMatchesBinary(receipt Receipt, runtimeDigest, modelDigest string, files []ModelFile, runner []byte) bool {
	expectedArtifact := lockedModelArtifactIdentity(modelDigest)
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
	if err := validateLockedRegularInfo(name, info, expectedSize); err != nil {
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
		verifyErr := verifyWheelInstallation(sitePackages, wheel.File, allowed)
		closeErr := wheel.Close()
		if err := errors.Join(verifyErr, closeErr); err != nil {
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
	defer func() { _ = reader.Close() }()
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
	file, before, err := openLockedRegular(name, 0)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	stableErr := verifyStableLockedFile(name, file, before, 0)
	closeErr := file.Close()
	if err := errors.Join(copyErr, stableErr, closeErr); err != nil {
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
	defer func() { _ = os.Remove(tmpName) }()
	defer func() { _ = tmp.Close() }()
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
	defer func() { _ = response.Body.Close() }()
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
	file, before, err := openLockedRegular(name, expectedSize)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	stableErr := verifyStableLockedFile(name, file, before, expectedSize)
	closeErr := file.Close()
	if err := errors.Join(copyErr, stableErr, closeErr); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("%s SHA-256 mismatch", filepath.Base(name))
	}
	return nil
}

func openLockedRegular(name string, expectedSize int64) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if err := validateLockedRegularInfo(name, before, expectedSize); err != nil {
		return nil, nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !os.SameFile(before, opened) || !sameLockedMetadata(before, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s changed while it was opened", filepath.Base(name))
	}
	if err := validateLockedRegularInfo(name, opened, expectedSize); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, before, nil
}

func verifyStableLockedFile(name string, file *os.File, before os.FileInfo, expectedSize int64) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	after, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if err := validateLockedRegularInfo(name, opened, expectedSize); err != nil {
		return err
	}
	if err := validateLockedRegularInfo(name, after, expectedSize); err != nil {
		return err
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) ||
		!sameLockedMetadata(before, opened) || !sameLockedMetadata(opened, after) {
		return fmt.Errorf("%s changed during verification", filepath.Base(name))
	}
	return nil
}

func validateLockedRegularInfo(name string, info os.FileInfo, expectedSize int64) error {
	if info == nil || !info.Mode().IsRegular() || expectedSize > 0 && info.Size() != expectedSize {
		return fmt.Errorf("%s does not match the locked regular file", filepath.Base(name))
	}
	links, ok := fileLinkCount(info)
	if !ok || links != 1 {
		return fmt.Errorf("%s must have exactly one filesystem link", filepath.Base(name))
	}
	return nil
}

func fileLinkCount(info os.FileInfo) (uint64, bool) {
	if info == nil {
		return 0, false
	}
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, false
	}
	field := value.FieldByName("Nlink")
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		count := field.Int()
		return uint64(count), count >= 0
	default:
		return 0, false
	}
}

func sameLockedMetadata(left, right os.FileInfo) bool {
	return left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
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
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
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
