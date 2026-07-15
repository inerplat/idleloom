package wirekubecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultReleaseBaseURL = "https://github.com/inerplat/wirekube"
	maxChecksumBytes      = 1 << 20
	maxBinaryBytes        = 128 << 20
)

type Resolver struct {
	Version        string
	BinaryOverride string
	CacheRoot      string
	ReleaseBaseURL string
	GOOS           string
	GOARCH         string
	HTTPClient     *http.Client
}

type receipt struct {
	Version string `json:"version"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
}

var verifyVersionHook = verifyVersion

func (r Resolver) Resolve(ctx context.Context) (string, error) {
	version := r.Version
	if version == "" {
		version = CompatibleVersion
	}
	if r.BinaryOverride != "" {
		if err := validateRegularExecutable(r.BinaryOverride); err != nil {
			return "", err
		}
		if err := verifyVersionHook(ctx, r.BinaryOverride, version); err != nil {
			return "", err
		}
		return r.BinaryOverride, nil
	}

	goos, goarch := r.GOOS, r.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if (goos != "darwin" && goos != "linux") || (goarch != "arm64" && goarch != "amd64") {
		return "", fmt.Errorf("the WireKube CLI has no supported release asset for %s/%s", goos, goarch)
	}
	asset := "wirekubectl-" + goos + "-" + goarch
	cacheRoot := r.CacheRoot
	if cacheRoot == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache directory: %w", err)
		}
		cacheRoot = filepath.Join(userCache, "idleloom", "dependencies")
	}
	directory := filepath.Join(cacheRoot, "wirekubectl", version)
	binaryPath := filepath.Join(directory, asset)
	receiptPath := binaryPath + ".json"
	if validCachedBinary(binaryPath, receiptPath, version, asset) {
		return binaryPath, nil
	}

	baseURL := strings.TrimSuffix(r.ReleaseBaseURL, "/")
	if baseURL == "" {
		baseURL = defaultReleaseBaseURL
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	releaseURL := baseURL + "/releases/download/" + version + "/"
	checksums, err := download(ctx, client, releaseURL+"wirekubectl-checksums.txt", maxChecksumBytes)
	if err != nil {
		return "", err
	}
	expected, err := checksumForAsset(string(checksums), asset)
	if err != nil {
		return "", err
	}
	binary, err := download(ctx, client, releaseURL+asset, maxBinaryBytes)
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(binary)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != expected {
		return "", fmt.Errorf("the WireKube CLI checksum mismatch: expected %s, got %s", expected, actualHex)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create WireKube dependency cache: %w", err)
	}
	if err := atomicWrite(binaryPath, binary, 0o700); err != nil {
		return "", err
	}
	receiptData, err := json.Marshal(receipt{Version: version, Asset: asset, SHA256: expected})
	if err != nil {
		return "", err
	}
	if err := atomicWrite(receiptPath, append(receiptData, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := verifyVersionHook(ctx, binaryPath, version); err != nil {
		_ = os.Remove(binaryPath)
		_ = os.Remove(receiptPath)
		return "", err
	}
	return binaryPath, nil
}

func verifyVersion(ctx context.Context, binary, expected string) error {
	info, err := (Client{Binary: binary}).Version(ctx)
	if err != nil {
		return err
	}
	if info.Version != expected {
		return fmt.Errorf("wirekubectl version %s is incompatible; Idleloom requires %s", info.Version, expected)
	}
	if !hasSHA256Digest(info.DefaultImage) {
		return fmt.Errorf("wirekubectl %s has no digest-pinned default image", info.Version)
	}
	return nil
}

func hasSHA256Digest(image string) bool {
	const separator = "@sha256:"
	index := strings.LastIndex(image, separator)
	if index <= 0 {
		return false
	}
	digest := image[index+len(separator):]
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func validCachedBinary(binaryPath, receiptPath, version, asset string) bool {
	if validateRegularExecutable(binaryPath) != nil {
		return false
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		return false
	}
	var stored receipt
	if json.Unmarshal(data, &stored) != nil || stored.Version != version || stored.Asset != asset || len(stored.SHA256) != 64 {
		return false
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(binary)
	return hex.EncodeToString(digest[:]) == stored.SHA256
}

func validateRegularExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect wirekubectl binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wirekubectl binary must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("wirekubectl binary is not executable")
	}
	return nil
}

func checksumForAsset(checksums, asset string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if len(fields[0]) != 64 {
			return "", fmt.Errorf("invalid checksum for %s", asset)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", fmt.Errorf("invalid checksum for %s: %w", asset, err)
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("the WireKube release checksums do not contain %s", asset)
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create WireKube release request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download WireKube release asset: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download WireKube release asset: HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download WireKube release asset: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the WireKube release asset exceeds the maximum allowed size")
	}
	return data, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".wirekubectl-*")
	if err != nil {
		return fmt.Errorf("create temporary dependency file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install WireKube dependency: %w", err)
	}
	return nil
}
