package devruntime

import (
	"bufio"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	LockedModelName = "qwen3-5-0-8b-mlx"
	ModelRepository = "mlx-community/Qwen3.5-0.8B-4bit"
	ModelRevision   = "da28692b5f139cb0ec58a356b437486b7dac7462"
	RuntimeVersion  = "mlx-lm-0.31.3"
)

type LockedModelDescriptor struct {
	Name             string
	Repository       string
	Revision         string
	ArtifactIdentity string
	ManifestDigest   string
	SizeBytes        int64
}

//go:embed assets/runtime.lock.tsv assets/model.lock.tsv runner.py
var embedded embed.FS

type RuntimeFile struct {
	Package string
	Version string
	URL     string
	SHA256  string
	Name    string
}

type ModelFile struct {
	Path   string
	Size   int64
	SHA256 string
}

func RuntimeLock() ([]RuntimeFile, string, error) {
	data, err := embedded.ReadFile("assets/runtime.lock.tsv")
	if err != nil {
		return nil, "", err
	}
	var files []RuntimeFile
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 4 {
			return nil, "", fmt.Errorf("invalid embedded runtime lock entry")
		}
		name := path.Base(fields[2])
		if name == "." || name == "/" || !strings.HasSuffix(name, ".whl") {
			return nil, "", fmt.Errorf("invalid wheel URL for %s", fields[0])
		}
		if err := validateDigest(fields[3]); err != nil {
			return nil, "", fmt.Errorf("runtime lock %s: %w", fields[0], err)
		}
		files = append(files, RuntimeFile{Package: fields[0], Version: fields[1], URL: fields[2], SHA256: fields[3], Name: name})
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("embedded runtime lock is empty")
	}
	return files, digest(data), nil
}

func ModelLock() ([]ModelFile, string, error) {
	data, err := embedded.ReadFile("assets/model.lock.tsv")
	if err != nil {
		return nil, "", err
	}
	var files []ModelFile
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || path.Base(fields[0]) != fields[0] {
			return nil, "", fmt.Errorf("invalid embedded model lock entry")
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size <= 0 {
			return nil, "", fmt.Errorf("invalid size for model file %s", fields[0])
		}
		if err := validateDigest(fields[2]); err != nil {
			return nil, "", fmt.Errorf("model lock %s: %w", fields[0], err)
		}
		files = append(files, ModelFile{Path: fields[0], Size: size, SHA256: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("embedded model lock is empty")
	}
	return files, digest(data), nil
}

func LockedModel() (LockedModelDescriptor, error) {
	files, digest, err := ModelLock()
	if err != nil {
		return LockedModelDescriptor{}, err
	}
	return lockedModelDescriptor(files, digest), nil
}

func lockedModelDescriptor(files []ModelFile, digest string) LockedModelDescriptor {
	var size int64
	for _, file := range files {
		size += file.Size
	}
	return LockedModelDescriptor{
		Name: LockedModelName, Repository: ModelRepository, Revision: ModelRevision,
		ArtifactIdentity: lockedModelArtifactIdentity(digest), ManifestDigest: "sha256:" + digest,
		SizeBytes: size,
	}
}

func lockedModelArtifactIdentity(modelDigest string) string {
	return "oci://development.invalid/idleloom/qwen3.5-0.8b-4bit@sha256:" + modelDigest
}

func RunnerSource() ([]byte, error) {
	return embedded.ReadFile("runner.py")
}

func validateDigest(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("SHA-256 must contain 64 lowercase hex characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("SHA-256 must contain 64 lowercase hex characters")
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
