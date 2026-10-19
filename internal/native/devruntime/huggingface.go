package devruntime

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHuggingFaceEndpoint = "https://huggingface.co"
	// MaxInlineHashBytes bounds the files stored without LFS that are hashed
	// by downloading them, because the tree API reports only a git blob ID for
	// those files and never their SHA-256.
	MaxInlineHashBytes = int64(16 << 20)

	hfReceiptVersion = 1
	maxHFSources     = 16
	maxHFConfigBytes = int64(16 << 20)
	maxHFModelFiles  = 4096
	maxHFTreePages   = 64
	maxHFTreePage    = int64(32 << 20)
	maxHFPathLength  = 1024
	maxHFRepository  = 192
	maxHFArtifact    = int64(64 << 30)
)

var (
	hfRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	hfRevisionPattern   = regexp.MustCompile(`^[a-f0-9]{40}$`)
	manifestDigestRegex = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	linkNextPattern     = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)
)

// HFModel identifies an MLX safetensors model at one Hugging Face commit and
// the manifest its files must hash to.
type HFModel struct {
	Repository     string
	Revision       string
	ManifestDigest string
	SizeBytes      int64
}

type HFSource struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// HFModelReceipt is written next to a prepared model directory once every
// file has been downloaded and checked against the manifest. One directory
// holds one manifest, so the receipt lists every commit verified to hash to
// it, such as a fork or a commit that changed only dotfiles.
type HFModelReceipt struct {
	Version        int         `json:"version"`
	ManifestDigest string      `json:"manifestDigest"`
	SizeBytes      int64       `json:"sizeBytes"`
	Sources        []HFSource  `json:"sources"`
	Files          []ModelFile `json:"files"`
}

// Serves reports whether this prepared directory was verified for model.
func (receipt HFModelReceipt) Serves(model HFModel) bool {
	if receipt.ManifestDigest != model.ManifestDigest || receipt.SizeBytes != model.SizeBytes {
		return false
	}
	for _, source := range receipt.Sources {
		if source.Repository == model.Repository && source.Revision == model.Revision {
			return true
		}
	}
	return false
}

func (receipt HFModelReceipt) Models() []HFModel {
	models := make([]HFModel, 0, len(receipt.Sources))
	for _, source := range receipt.Sources {
		models = append(models, HFModel{Repository: source.Repository, Revision: source.Revision, ManifestDigest: receipt.ManifestDigest, SizeBytes: receipt.SizeBytes})
	}
	return models
}

// hfFile is one file listed by the Hugging Face tree API. SHA256 is known up
// front only for LFS files.
type hfFile struct {
	Path    string
	Size    int64
	SHA256  string
	BlobOID string
	LFS     bool
}

func ValidateHFModel(model HFModel) error {
	if err := validateHFSource(model.Repository, model.Revision); err != nil {
		return err
	}
	if !manifestDigestRegex.MatchString(model.ManifestDigest) {
		return fmt.Errorf("manifest digest %q must be sha256:<64 lowercase hex characters>", model.ManifestDigest)
	}
	if model.SizeBytes <= 0 || model.SizeBytes > maxHFArtifact {
		return fmt.Errorf("model size %d is outside 1 to %d bytes", model.SizeBytes, maxHFArtifact)
	}
	return nil
}

func validateHFSource(repository, revision string) error {
	if len(repository) > maxHFRepository || !hfRepositoryPattern.MatchString(repository) {
		return fmt.Errorf("Hugging Face repository %q must be OWNER/NAME", repository)
	}
	if !hfRevisionPattern.MatchString(revision) {
		return fmt.Errorf("Hugging Face revision %q must be a full 40-character lowercase commit SHA", revision)
	}
	return nil
}

// inMLXManifest reports whether a repository file belongs to the manifest:
// every regular file whose base name does not start with a dot.
func inMLXManifest(name string) bool {
	return !strings.HasPrefix(path.Base(name), ".")
}

// mlxManifest is the canonical manifest text. Files are sorted by path in
// byte order and each contributes exactly "path\tsize\tsha256hex\n"; there is
// no header or trailer. The manifest digest is "sha256:" followed by the
// SHA-256 of this text.
func mlxManifest(files []ModelFile) ([]byte, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("the model manifest has no files")
	}
	if len(files) > maxHFModelFiles {
		return nil, fmt.Errorf("the model manifest has %d files, more than %d", len(files), maxHFModelFiles)
	}
	sorted := append([]ModelFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	if err := validateManifestPaths(sorted); err != nil {
		return nil, err
	}
	var text bytes.Buffer
	for _, file := range sorted {
		if file.Size < 0 {
			return nil, fmt.Errorf("model file %s has a negative size", file.Path)
		}
		if err := validateDigest(file.SHA256); err != nil {
			return nil, fmt.Errorf("model file %s: %w", file.Path, err)
		}
		text.WriteString(file.Path)
		text.WriteByte('\t')
		text.WriteString(strconv.FormatInt(file.Size, 10))
		text.WriteByte('\t')
		text.WriteString(file.SHA256)
		text.WriteByte('\n')
	}
	return text.Bytes(), nil
}

func MLXManifestDigest(files []ModelFile) (string, error) {
	text, err := mlxManifest(files)
	if err != nil {
		return "", err
	}
	return "sha256:" + digest(text), nil
}

func manifestSize(files []ModelFile) int64 {
	var total int64
	for _, file := range files {
		total += file.Size
	}
	return total
}

// validateHFPath accepts only clean relative paths of printable ASCII whose
// every component can be created as an ordinary directory entry.
func validateHFPath(name string) error {
	if name == "" || len(name) > maxHFPathLength || path.IsAbs(name) || path.Clean(name) != name {
		return fmt.Errorf("model file path %q is not a clean relative path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 {
			return fmt.Errorf("model file path %q has an unsafe component", name)
		}
		for i := 0; i < len(part); i++ {
			if c := part[i]; c < 0x20 || c > 0x7e || c == '\\' || c == ':' {
				return fmt.Errorf("model file path %q contains an unsupported character", name)
			}
		}
	}
	return nil
}

// validateManifestPaths rejects duplicates and names that collide on a
// case-insensitive filesystem, including a file colliding with a directory.
func validateManifestPaths(sorted []ModelFile) error {
	files := make(map[string]string, len(sorted))
	directories := make(map[string]string)
	for _, file := range sorted {
		if err := validateHFPath(file.Path); err != nil {
			return err
		}
		if !inMLXManifest(file.Path) {
			return fmt.Errorf("model file %s is a dotfile and cannot be in the manifest", file.Path)
		}
		folded := strings.ToLower(file.Path)
		if other, ok := files[folded]; ok {
			return fmt.Errorf("model files %s and %s collide", other, file.Path)
		}
		if other, ok := directories[folded]; ok {
			return fmt.Errorf("model file %s collides with directory %s", file.Path, other)
		}
		files[folded] = file.Path
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			foldedParent := strings.ToLower(parent)
			if other, ok := files[foldedParent]; ok {
				return fmt.Errorf("model file %s collides with directory %s", other, parent)
			}
			if other, ok := directories[foldedParent]; ok && other != parent {
				return fmt.Errorf("model directories %s and %s collide", other, parent)
			}
			directories[foldedParent] = parent
		}
	}
	return nil
}

type hfTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// listHFFiles returns the manifest files of a repository commit as the tree
// API reports them, following pagination only on the same endpoint.
func listHFFiles(ctx context.Context, client *http.Client, endpoint, repository, revision string) ([]hfFile, error) {
	if err := validateHFSource(repository, revision); err != nil {
		return nil, err
	}
	base, err := hfEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	next := base.JoinPath("api", "models", repository, "tree", revision)
	next.RawQuery = "recursive=true"
	var files []hfFile
	for page := 0; next != nil; page++ {
		if page >= maxHFTreePages {
			return nil, fmt.Errorf("Hugging Face tree listing exceeded %d pages", maxHFTreePages)
		}
		entries, link, err := fetchHFTreePage(ctx, client, next.String())
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			switch entry.Type {
			case "directory":
				continue
			case "file":
			default:
				return nil, fmt.Errorf("Hugging Face tree entry %q has unsupported type %q", entry.Path, entry.Type)
			}
			if !inMLXManifest(entry.Path) {
				continue
			}
			if err := validateHFPath(entry.Path); err != nil {
				return nil, err
			}
			file := hfFile{Path: entry.Path, Size: entry.Size, BlobOID: entry.OID}
			if entry.LFS != nil {
				if err := validateDigest(entry.LFS.OID); err != nil {
					return nil, fmt.Errorf("LFS file %s: %w", entry.Path, err)
				}
				if entry.LFS.Size != entry.Size && entry.Size != 0 {
					return nil, fmt.Errorf("LFS file %s reports sizes %d and %d", entry.Path, entry.Size, entry.LFS.Size)
				}
				file.Size, file.SHA256, file.LFS = entry.LFS.Size, entry.LFS.OID, true
			}
			if file.Size < 0 {
				return nil, fmt.Errorf("model file %s has a negative size", entry.Path)
			}
			files = append(files, file)
			if len(files) > maxHFModelFiles {
				return nil, fmt.Errorf("the repository has more than %d model files", maxHFModelFiles)
			}
		}
		next = nil
		if link != "" {
			candidate, err := base.Parse(link)
			if err != nil || candidate.Scheme != base.Scheme || candidate.Host != base.Host {
				return nil, fmt.Errorf("Hugging Face tree pagination left the endpoint")
			}
			next = candidate
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s@%s has no model files", repository, revision)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func fetchHFTreePage(ctx context.Context, client *http.Client, target string) ([]hfTreeEntry, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("list Hugging Face files: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("list Hugging Face files: unexpected HTTP status %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxHFTreePage+1))
	if err != nil {
		return nil, "", fmt.Errorf("list Hugging Face files: %w", err)
	}
	if int64(len(data)) > maxHFTreePage {
		return nil, "", fmt.Errorf("list Hugging Face files: tree page exceeds %d bytes", maxHFTreePage)
	}
	var entries []hfTreeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, "", fmt.Errorf("decode Hugging Face tree listing: %w", err)
	}
	var next string
	for _, link := range response.Header.Values("Link") {
		if match := linkNextPattern.FindStringSubmatch(link); match != nil {
			next = match[1]
		}
	}
	return entries, next, nil
}

// HFManifest is a repository commit's manifest files, sorted by path, with
// their digest and total size.
type HFManifest struct {
	Files     []ModelFile
	Digest    string
	SizeBytes int64
}

// ResolveHFManifest lists a repository commit and returns its manifest. LFS
// files use the SHA-256 the tree API reports; files stored without LFS are
// downloaded, checked against their git blob ID, and hashed, which is only done
// up to MaxInlineHashBytes.
func ResolveHFManifest(ctx context.Context, client *http.Client, endpoint, repository, revision string) (HFManifest, error) {
	listed, err := listHFFiles(ctx, client, endpoint, repository, revision)
	if err != nil {
		return HFManifest{}, err
	}
	files := make([]ModelFile, 0, len(listed))
	hasConfig := false
	for _, file := range listed {
		hasConfig = hasConfig || file.Path == "config.json"
		if file.LFS {
			files = append(files, ModelFile{Path: file.Path, Size: file.Size, SHA256: file.SHA256})
			continue
		}
		if file.Size > MaxInlineHashBytes {
			return HFManifest{}, fmt.Errorf("model file %s is %d bytes and not stored with LFS; only files up to %d bytes can be hashed without LFS metadata", file.Path, file.Size, MaxInlineHashBytes)
		}
		data, err := fetchHFFile(ctx, client, endpoint, repository, revision, file)
		if err != nil {
			return HFManifest{}, err
		}
		if file.Path == "config.json" {
			if err := checkMLXConfig(data); err != nil {
				return HFManifest{}, err
			}
		}
		sum := sha256.Sum256(data)
		files = append(files, ModelFile{Path: file.Path, Size: file.Size, SHA256: hex.EncodeToString(sum[:])})
	}
	if !hasConfig {
		return HFManifest{}, fmt.Errorf("%s@%s has no config.json and is not an MLX model", repository, revision)
	}
	digest, err := MLXManifestDigest(files)
	if err != nil {
		return HFManifest{}, err
	}
	return HFManifest{Files: files, Digest: digest, SizeBytes: manifestSize(files)}, nil
}

// checkMLXConfig refuses a config.json naming a model_file: the locked mlx-lm
// executes that Python module while loading, which would turn a pinned data
// artifact into code.
func checkMLXConfig(data []byte) error {
	var config any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode config.json: %w", err)
	}
	if _, ok := config.(map[string]any); !ok {
		return fmt.Errorf("config.json is not a JSON object")
	}
	var walk func(any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			for key, nested := range typed {
				if key == "model_file" {
					return fmt.Errorf("config.json names a model_file, which the runtime would execute as code; such models are refused")
				}
				if err := walk(nested); err != nil {
					return err
				}
			}
		case []any:
			for _, nested := range typed {
				if err := walk(nested); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(config)
}

func fetchHFFile(ctx context.Context, client *http.Client, endpoint, repository, revision string, file hfFile) ([]byte, error) {
	target, err := hfResolveURL(endpoint, repository, revision, file.Path)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", file.Path, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: unexpected HTTP status %s", file.Path, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, file.Size+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", file.Path, err)
	}
	if int64(len(data)) != file.Size {
		return nil, fmt.Errorf("download %s: got %d bytes, the tree lists %d", file.Path, len(data), file.Size)
	}
	if hfRevisionPattern.MatchString(file.BlobOID) {
		blob := sha1.New()
		_, _ = fmt.Fprintf(blob, "blob %d\x00", len(data))
		_, _ = blob.Write(data)
		if hex.EncodeToString(blob.Sum(nil)) != file.BlobOID {
			return nil, fmt.Errorf("download %s: content does not match its git blob ID", file.Path)
		}
	}
	return data, nil
}

func hfEndpoint(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		endpoint = DefaultHuggingFaceEndpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("invalid Hugging Face endpoint %q", endpoint)
	}
	return base, nil
}

func hfResolveURL(endpoint, repository, revision, name string) (string, error) {
	base, err := hfEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	if err := validateHFPath(name); err != nil {
		return "", err
	}
	segments := strings.Split(name, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	target, err := base.Parse(path.Join("/", base.EscapedPath(), repository, "resolve", revision) + "/" + strings.Join(segments, "/"))
	if err != nil {
		return "", err
	}
	target.RawQuery = "download=true"
	return target.String(), nil
}

// HFModelDirectory is the per-model directory, keyed by manifest digest so a
// manifest is stored once however many commits or catalog entries name it.
func HFModelDirectory(layout Layout, manifestDigest string) string {
	return filepath.Join(layout.HFModels, strings.TrimPrefix(manifestDigest, "sha256:"))
}

func hfReceiptPath(layout Layout, manifestDigest string) string {
	return HFModelDirectory(layout, manifestDigest) + ".json"
}

// PrepareHFModel prepares the locked runtime and downloads one pinned model.
// It refuses the model before downloading weights when the commit's files do
// not hash to the declared manifest or add up to the declared size, and it
// never writes outside the model's own directory and receipt.
func (p Preparer) PrepareHFModel(ctx context.Context, model HFModel) (HFModelReceipt, error) {
	if err := ValidateHFModel(model); err != nil {
		return HFModelReceipt{}, err
	}
	if _, err := p.PrepareRuntime(ctx); err != nil {
		return HFModelReceipt{}, err
	}
	layout := NewLayout(p.root())
	runner, err := RunnerSource()
	if err != nil {
		return HFModelReceipt{}, err
	}
	if err := atomicWrite(layout.Runner, runner, 0o500); err != nil {
		return HFModelReceipt{}, fmt.Errorf("write embedded runner: %w", err)
	}
	return p.downloadHFModel(ctx, layout, model)
}

func (p Preparer) downloadHFModel(ctx context.Context, layout Layout, model HFModel) (HFModelReceipt, error) {
	client := p.HTTPClient
	if client == nil {
		// Weights run to many gigabytes; the bound only ends a stalled transfer.
		client = &http.Client{Timeout: 4 * time.Hour}
	}
	p.progress("model listing " + model.Repository + "@" + model.Revision)
	manifest, err := ResolveHFManifest(ctx, client, p.HFEndpoint, model.Repository, model.Revision)
	if err != nil {
		return HFModelReceipt{}, err
	}
	if manifest.Digest != model.ManifestDigest {
		return HFModelReceipt{}, fmt.Errorf("%s@%s has manifest digest %s, not the pinned %s", model.Repository, model.Revision, manifest.Digest, model.ManifestDigest)
	}
	if manifest.SizeBytes != model.SizeBytes {
		return HFModelReceipt{}, fmt.Errorf("%s@%s has %d bytes of model files, not the pinned %d", model.Repository, model.Revision, manifest.SizeBytes, model.SizeBytes)
	}
	files := manifest.Files
	if err := os.MkdirAll(layout.HFModels, 0o700); err != nil {
		return HFModelReceipt{}, fmt.Errorf("create model directory: %w", err)
	}
	if err := os.Chmod(layout.HFModels, 0o700); err != nil {
		return HFModelReceipt{}, fmt.Errorf("restrict model directory: %w", err)
	}
	directory := HFModelDirectory(layout, model.ManifestDigest)
	if err := ensurePlainDirectory(directory); err != nil {
		return HFModelReceipt{}, err
	}
	if err := removeUnlisted(directory, files); err != nil {
		return HFModelReceipt{}, err
	}
	for _, file := range files {
		if err := ensureModelParents(directory, file.Path); err != nil {
			return HFModelReceipt{}, err
		}
		p.progress("model " + file.Path)
		target, err := hfResolveURL(p.HFEndpoint, model.Repository, model.Revision, file.Path)
		if err != nil {
			return HFModelReceipt{}, err
		}
		if err := ensureDownload(ctx, client, target, filepath.Join(directory, filepath.FromSlash(file.Path)), file.Size, file.SHA256, file.Size); err != nil {
			return HFModelReceipt{}, err
		}
	}
	if err := removeUnlisted(directory, files); err != nil {
		return HFModelReceipt{}, err
	}
	if err := checkMLXConfigFile(directory); err != nil {
		return HFModelReceipt{}, err
	}
	sources := []HFSource{{Repository: model.Repository, Revision: model.Revision}}
	if previous, err := readHFReceipt(hfReceiptPath(layout, model.ManifestDigest)); err == nil && previous.ManifestDigest == model.ManifestDigest {
		for _, source := range previous.Sources {
			if source != sources[0] {
				sources = append(sources, source)
			}
		}
	}
	if len(sources) > maxHFSources {
		sources = sources[:maxHFSources]
	}
	receipt := HFModelReceipt{
		Version: hfReceiptVersion, ManifestDigest: model.ManifestDigest, SizeBytes: model.SizeBytes,
		Sources: sources, Files: files,
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return HFModelReceipt{}, err
	}
	if err := atomicWrite(hfReceiptPath(layout, model.ManifestDigest), append(data, '\n'), 0o600); err != nil {
		return HFModelReceipt{}, fmt.Errorf("write model receipt: %w", err)
	}
	return receipt, nil
}

func ensurePlainDirectory(name string) error {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(name, 0o700); err != nil {
			return fmt.Errorf("create model directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a plain directory", filepath.Base(name))
	}
	return nil
}

func ensureModelParents(directory, name string) error {
	current := directory
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		if err := ensurePlainDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

// removeUnlisted deletes anything inside the model directory that is not part
// of its manifest, such as an interrupted download, so the runtime can never
// load a weight file the manifest does not name.
func removeUnlisted(directory string, files []ModelFile) error {
	expectedFiles, expectedDirectories := manifestEntries(files)
	return filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, ok := expectedDirectories[relative]; ok {
				return nil
			}
			if err := os.RemoveAll(name); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if _, ok := expectedFiles[relative]; ok && entry.Type().IsRegular() {
			return nil
		}
		return os.Remove(name)
	})
}

func manifestEntries(files []ModelFile) (map[string]ModelFile, map[string]struct{}) {
	expectedFiles := make(map[string]ModelFile, len(files))
	expectedDirectories := make(map[string]struct{})
	for _, file := range files {
		expectedFiles[file.Path] = file
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			expectedDirectories[parent] = struct{}{}
		}
	}
	return expectedFiles, expectedDirectories
}

// VerifyHFModel checks the runtime, the runner, and every byte of a prepared
// model against its receipt. Process startup must call it.
func VerifyHFModel(layout Layout, model HFModel) (HFModelReceipt, error) {
	return verifyHFModelWithRuntime(layout, model, true)
}

// VerifyHFModelFast checks the same receipts and the model directory's exact
// file shape without rereading the weights.
func VerifyHFModelFast(layout Layout, model HFModel) (HFModelReceipt, error) {
	return verifyHFModelWithRuntime(layout, model, false)
}

func verifyHFModelWithRuntime(layout Layout, model HFModel, strong bool) (HFModelReceipt, error) {
	if _, err := verifyRuntime(layout, strong); err != nil {
		return HFModelReceipt{}, err
	}
	runner, err := RunnerSource()
	if err != nil {
		return HFModelReceipt{}, err
	}
	if err := verifyFile(layout.Runner, digest(runner), int64(len(runner))); err != nil {
		return HFModelReceipt{}, fmt.Errorf("verify embedded runner: %w", err)
	}
	return verifyHFModel(layout, model, strong)
}

func verifyHFModel(layout Layout, model HFModel, strong bool) (HFModelReceipt, error) {
	if err := ValidateHFModel(model); err != nil {
		return HFModelReceipt{}, err
	}
	receipt, err := readHFReceipt(hfReceiptPath(layout, model.ManifestDigest))
	if err != nil {
		return HFModelReceipt{}, err
	}
	if !receipt.Serves(model) {
		return HFModelReceipt{}, fmt.Errorf("the prepared model receipt does not cover %s@%s", model.Repository, model.Revision)
	}
	directory := HFModelDirectory(layout, model.ManifestDigest)
	if err := verifyHFModelDirectory(directory, receipt.Files, strong); err != nil {
		return HFModelReceipt{}, err
	}
	if err := checkMLXConfigFile(directory); err != nil {
		return HFModelReceipt{}, err
	}
	return receipt, nil
}

func checkMLXConfigFile(directory string) error {
	name := filepath.Join(directory, "config.json")
	info, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect config.json: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxHFConfigBytes {
		return fmt.Errorf("config.json is not a bounded regular file")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return checkMLXConfig(data)
}

func readHFReceipt(name string) (HFModelReceipt, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return HFModelReceipt{}, fmt.Errorf("read model receipt: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return HFModelReceipt{}, fmt.Errorf("model receipt %s is not a bounded regular file", filepath.Base(name))
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return HFModelReceipt{}, fmt.Errorf("read model receipt: %w", err)
	}
	var receipt HFModelReceipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return HFModelReceipt{}, fmt.Errorf("decode model receipt: %w", err)
	}
	if receipt.Version != hfReceiptVersion {
		return HFModelReceipt{}, fmt.Errorf("model receipt version %d is not supported", receipt.Version)
	}
	if len(receipt.Sources) == 0 || len(receipt.Sources) > maxHFSources {
		return HFModelReceipt{}, fmt.Errorf("model receipt must list 1 to %d sources", maxHFSources)
	}
	for _, model := range receipt.Models() {
		if err := ValidateHFModel(model); err != nil {
			return HFModelReceipt{}, err
		}
	}
	actual, err := MLXManifestDigest(receipt.Files)
	if err != nil {
		return HFModelReceipt{}, err
	}
	if actual != receipt.ManifestDigest || manifestSize(receipt.Files) != receipt.SizeBytes {
		return HFModelReceipt{}, fmt.Errorf("model receipt files do not match its manifest digest and size")
	}
	return receipt, nil
}

// verifyHFModelDirectory requires the directory to hold exactly the manifest:
// plain directories, and single-link regular files of the recorded size, with
// no symbolic links anywhere. strong also rehashes every file.
func verifyHFModelDirectory(directory string, files []ModelFile, strong bool) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect model directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the model directory is not a plain directory")
	}
	expectedFiles, expectedDirectories := manifestEntries(files)
	seen := 0
	err = filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, ok := expectedDirectories[relative]; !ok {
				return fmt.Errorf("unexpected directory %s in the model directory", relative)
			}
			return nil
		}
		file, ok := expectedFiles[relative]
		if !ok {
			return fmt.Errorf("unexpected file %s in the model directory", relative)
		}
		fileInfo, err := os.Lstat(name)
		if err != nil {
			return err
		}
		if err := validateLockedRegularInfo(name, fileInfo, file.Size); err != nil {
			return err
		}
		if fileInfo.Size() != file.Size {
			return fmt.Errorf("%s does not have its recorded size", relative)
		}
		if strong {
			if err := verifyFile(name, file.SHA256, file.Size); err != nil {
				return err
			}
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(expectedFiles) {
		return fmt.Errorf("the model directory is missing %d manifest files", len(expectedFiles)-seen)
	}
	return nil
}

// DiscoverHFModels lists the prepared models whose receipt and directory
// shape still check out, without rehashing weights, for host advertisement.
func DiscoverHFModels(layout Layout) []HFModelReceipt {
	entries, err := os.ReadDir(layout.HFModels)
	if err != nil {
		return nil
	}
	var receipts []HFModelReceipt
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}
		manifestDigest := "sha256:" + strings.TrimSuffix(name, ".json")
		if !manifestDigestRegex.MatchString(manifestDigest) {
			continue
		}
		receipt, err := readHFReceipt(filepath.Join(layout.HFModels, name))
		if err != nil || receipt.ManifestDigest != manifestDigest {
			continue
		}
		if _, err := verifyHFModel(layout, receipt.Models()[0], false); err != nil {
			continue
		}
		receipts = append(receipts, receipt)
		if len(receipts) == 64 {
			break
		}
	}
	return receipts
}
