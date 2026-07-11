package idleloom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var kubernetesVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func DownloadKubelet(ctx context.Context, version string) (string, error) {
	normalized, err := normalizeKubernetesVersion(version)
	if err != nil {
		return "", err
	}
	version = normalized
	if !kubernetesVersionPattern.MatchString(version) {
		return "", fmt.Errorf("unsupported Kubernetes version %q", version)
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	dir := filepath.Join(cacheRoot, "idleloom", "kubernetes", version, "linux-arm64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create kubelet cache: %w", err)
	}
	destination := filepath.Join(dir, "kubelet")
	baseURL := "https://dl.k8s.io/release/" + version + "/bin/linux/arm64/kubelet"
	expected, err := fetchChecksum(ctx, baseURL+".sha256")
	if err != nil {
		return "", err
	}
	if actual, err := fileSHA256(destination); err == nil && actual == expected {
		return destination, nil
	}

	temporary, err := os.CreateTemp(dir, ".kubelet-*")
	if err != nil {
		return "", fmt.Errorf("create kubelet download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		temporary.Close()
		return "", fmt.Errorf("create kubelet request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		temporary.Close()
		return "", fmt.Errorf("download kubelet %s: %w", version, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		temporary.Close()
		return "", fmt.Errorf("download kubelet %s: HTTP %s", version, response.Status)
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hash), response.Body); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write kubelet download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close kubelet download: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return "", fmt.Errorf("kubelet checksum mismatch: expected %s, got %s", expected, actual)
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		return "", fmt.Errorf("mark kubelet executable: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("cache kubelet: %w", err)
	}
	return destination, nil
}

func fetchChecksum(ctx context.Context, url string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create checksum request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("download kubelet checksum: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download kubelet checksum: HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", fmt.Errorf("read kubelet checksum: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", fmt.Errorf("invalid kubelet checksum response")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", fmt.Errorf("invalid kubelet checksum: %w", err)
	}
	return strings.ToLower(fields[0]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
