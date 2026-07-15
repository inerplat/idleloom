package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"golang.org/x/sys/unix"
)

const SchemaV1Alpha1 = "native.ai.idleloom.io/artifact-manifest/v1alpha1"

type FileType string

const (
	FileTypeRegular   FileType = "regular"
	FileTypeSymlink   FileType = "symlink"
	FileTypeHardlink  FileType = "hardlink"
	FileTypeDirectory FileType = "directory"
)

type File struct {
	Path      string   `json:"path"`
	Type      FileType `json:"type"`
	SizeBytes int64    `json:"sizeBytes"`
	SHA256    string   `json:"sha256"`
	Mode      uint32   `json:"mode"`
}

type Manifest struct {
	SchemaVersion  string `json:"schemaVersion"`
	TotalSizeBytes int64  `json:"totalSizeBytes"`
	Files          []File `json:"files"`
}

type Policy struct {
	MaxFiles           int
	MaxFileBytes       int64
	MaxTotalBytes      int64
	AllowedExtensions  map[string]struct{}
	RequiredPaths      map[string]struct{}
	RequireSafetensors bool
}

var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var safePathPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)

const maxManifestBytes = 1 << 20

func DefaultPolicy() Policy {
	return Policy{
		MaxFiles:      256,
		MaxFileBytes:  64 << 30,
		MaxTotalBytes: 96 << 30,
		AllowedExtensions: map[string]struct{}{
			".json":        {},
			".safetensors": {},
		},
		RequiredPaths: map[string]struct{}{
			"config.json":    {},
			"tokenizer.json": {},
		},
		RequireSafetensors: true,
	}
}

// ValidateDeclaration validates an untrusted manifest declaration. It does not
// pull OCI content, verify a signature, or hash extracted file bytes.
func (p Policy) ValidateDeclaration(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaV1Alpha1 {
		return fmt.Errorf("unsupported artifact manifest schema %q", manifest.SchemaVersion)
	}
	if len(manifest.Files) == 0 {
		return fmt.Errorf("artifact manifest has no files")
	}
	if p.MaxFiles <= 0 || len(manifest.Files) > p.MaxFiles {
		return fmt.Errorf("artifact contains %d files; maximum is %d", len(manifest.Files), p.MaxFiles)
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	foundRequiredPaths := make(map[string]struct{}, len(p.RequiredPaths))
	hasSafetensors := false
	var total int64
	for _, file := range manifest.Files {
		if err := p.validateFile(file); err != nil {
			return fmt.Errorf("artifact file %q: %w", file.Path, err)
		}
		if _, exists := seen[file.Path]; exists {
			return fmt.Errorf("artifact contains duplicate path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if _, required := p.RequiredPaths[file.Path]; required {
			foundRequiredPaths[file.Path] = struct{}{}
		}
		if strings.EqualFold(path.Ext(file.Path), ".safetensors") {
			hasSafetensors = true
		}
		if total > p.MaxTotalBytes-file.SizeBytes {
			return fmt.Errorf("artifact size exceeds maximum %d bytes", p.MaxTotalBytes)
		}
		total += file.SizeBytes
	}
	if total != manifest.TotalSizeBytes {
		return fmt.Errorf("artifact total size is %d bytes, manifest declares %d", total, manifest.TotalSizeBytes)
	}
	if total > p.MaxTotalBytes {
		return fmt.Errorf("artifact size %d exceeds maximum %d bytes", total, p.MaxTotalBytes)
	}
	for required := range p.RequiredPaths {
		if _, found := foundRequiredPaths[required]; !found {
			return fmt.Errorf("artifact is missing required file %q", required)
		}
	}
	if p.RequireSafetensors && !hasSafetensors {
		return fmt.Errorf("artifact has no safetensors weights")
	}
	return nil
}

// VerifyManifestBlob hashes and strictly decodes the actual manifest bytes,
// then binds their declared content to the curated catalog entry.
func (p Policy) VerifyManifestBlob(catalog nativev1alpha1.ModelArtifact, data []byte) (Manifest, error) {
	if len(data) == 0 || len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("artifact manifest size must be between 1 and %d bytes", maxManifestBytes)
	}
	digest := sha256.Sum256(data)
	actualDigest := "sha256:" + hex.EncodeToString(digest[:])
	if actualDigest != catalog.ManifestDigest {
		return Manifest{}, fmt.Errorf("artifact manifest digest %s does not match catalog digest %s", actualDigest, catalog.ManifestDigest)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode artifact manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Manifest{}, fmt.Errorf("decode artifact manifest: trailing data")
	}
	if err := p.ValidateDeclaration(manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.TotalSizeBytes != catalog.SizeBytes {
		return Manifest{}, fmt.Errorf("artifact size %d does not match catalog size %d", manifest.TotalSizeBytes, catalog.SizeBytes)
	}
	return manifest, nil
}

// VerifyExtractedTree checks the actual staging tree without following links.
// OCI signature verification must happen before this function is called.
func (p Policy) VerifyExtractedTree(root string, manifest Manifest) error {
	if err := p.ValidateDeclaration(manifest); err != nil {
		return err
	}
	rootDescriptor, rootStat, err := openPrivateStagingRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootDescriptor) }()
	expectedFiles := make(map[string]File, len(manifest.Files))
	expectedDirs := make(map[string]struct{})
	for _, file := range manifest.Files {
		expectedFiles[file.Path] = file
		for directory := path.Dir(file.Path); directory != "."; directory = path.Dir(directory) {
			expectedDirs[directory] = struct{}{}
			if directory == path.Dir(directory) {
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(expectedFiles))
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			if !entry.IsDir() {
				return fmt.Errorf("artifact root is not a directory")
			}
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, expected := expectedDirs[relative]; !expected {
				return fmt.Errorf("artifact contains undeclared directory %q", relative)
			}
			return nil
		}
		expected, found := expectedFiles[relative]
		if !found {
			return fmt.Errorf("artifact contains undeclared file %q", relative)
		}
		if err := verifyRegularFileAt(rootDescriptor, relative, expected); err != nil {
			return fmt.Errorf("verify artifact file %q: %w", relative, err)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if err := verifyStagingRootIdentity(root, rootStat); err != nil {
		return err
	}
	for file := range expectedFiles {
		if _, found := seen[file]; !found {
			return fmt.Errorf("artifact is missing declared file %q", file)
		}
	}
	return nil
}

func openPrivateStagingRoot(root string) (int, unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(root, &stat); err != nil {
		return -1, stat, fmt.Errorf("inspect artifact staging root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, stat, fmt.Errorf("artifact staging root is not a directory")
	}
	if uint32(stat.Mode)&0o7777 != 0o700 {
		return -1, stat, fmt.Errorf("artifact staging root mode must be 0700")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return -1, stat, fmt.Errorf("artifact staging root is owned by UID %d, want %d", stat.Uid, os.Geteuid())
	}
	descriptor, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, stat, fmt.Errorf("open artifact staging root: %w", err)
	}
	return descriptor, stat, nil
}

func verifyStagingRootIdentity(root string, expected unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Lstat(root, &current); err != nil {
		return fmt.Errorf("reinspect artifact staging root: %w", err)
	}
	if current.Dev != expected.Dev || current.Ino != expected.Ino {
		return fmt.Errorf("artifact staging root changed during verification")
	}
	return nil
}

func verifyRegularFileAt(rootDescriptor int, relative string, expected File) error {
	components := strings.Split(relative, "/")
	directory, err := unix.Dup(rootDescriptor)
	if err != nil {
		return err
	}
	for _, component := range components[:len(components)-1] {
		next, err := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		closeErr := unix.Close(directory)
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if closeErr != nil {
			_ = unix.Close(next)
			return closeErr
		}
		directory = next
	}
	descriptor, err := unix.Openat(directory, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	closeErr := unix.Close(directory)
	if err != nil {
		return errors.Join(err, closeErr)
	}
	if closeErr != nil {
		_ = unix.Close(descriptor)
		return closeErr
	}
	file := os.NewFile(uintptr(descriptor), relative)
	if file == nil {
		_ = unix.Close(descriptor)
		return fmt.Errorf("open file descriptor")
	}
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("not a regular file")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("hard-linked files are not allowed")
	}
	if stat.Size != expected.SizeBytes {
		return fmt.Errorf("size %d does not match manifest size %d", stat.Size, expected.SizeBytes)
	}
	mode := uint32(stat.Mode) & 0o7777
	if mode != 0o600 && mode != 0o644 {
		return fmt.Errorf("mode %#o is not allowed", mode)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if digest != expected.SHA256 {
		return fmt.Errorf("digest %s does not match manifest digest %s", digest, expected.SHA256)
	}
	return nil
}

func (p Policy) validateFile(file File) error {
	if file.Type != FileTypeRegular {
		return fmt.Errorf("file type %q is not allowed", file.Type)
	}
	if file.Path == "" || strings.ContainsRune(file.Path, '\x00') || strings.Contains(file.Path, "\\") || strings.HasPrefix(file.Path, "/") {
		return fmt.Errorf("path must be a non-empty relative slash-separated path")
	}
	if !safePathPattern.MatchString(file.Path) {
		return fmt.Errorf("path must contain lowercase ASCII letters, digits, dots, dashes, underscores, and slashes only")
	}
	clean := path.Clean(file.Path)
	if clean != file.Path || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path is not canonical")
	}
	if file.Mode != 0 && file.Mode != 0o600 && file.Mode != 0o644 {
		return fmt.Errorf("mode %#o is not allowed", file.Mode)
	}
	if file.SizeBytes <= 0 {
		return fmt.Errorf("size must be positive")
	}
	if p.MaxFileBytes <= 0 || file.SizeBytes > p.MaxFileBytes {
		return fmt.Errorf("size %d exceeds maximum %d bytes", file.SizeBytes, p.MaxFileBytes)
	}
	if !sha256Pattern.MatchString(file.SHA256) {
		return fmt.Errorf("digest must use sha256:<64 lowercase hex characters>")
	}
	extension := strings.ToLower(path.Ext(file.Path))
	if _, allowed := p.AllowedExtensions[extension]; !allowed {
		return fmt.Errorf("extension %q is not allowed", extension)
	}
	return nil
}
