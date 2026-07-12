package devruntime

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestReceiptMatchesBinaryPinsArtifactProvenance(t *testing.T) {
	runner := []byte("runner")
	files := []ModelFile{{Path: "model.safetensors", Size: 1, SHA256: hex.EncodeToString(make([]byte, sha256.Size))}}
	runtimeDigest := hex.EncodeToString(bytes.Repeat([]byte{1}, sha256.Size))
	modelDigest := hex.EncodeToString(bytes.Repeat([]byte{2}, sha256.Size))
	receipt := Receipt{
		Version: receiptVersion, DevelopmentOnly: true,
		ArtifactIdentity:  "oci://development.invalid/idleloom/qwen3.5-0.8b-4bit@sha256:" + modelDigest,
		ManifestDigest:    "sha256:" + modelDigest,
		RuntimeLockDigest: "sha256:" + runtimeDigest,
		ModelRepository:   ModelRepository,
		ModelRevision:     ModelRevision,
		RuntimeVersion:    RuntimeVersion,
		RunnerDigest:      "sha256:" + digest(runner),
		ModelFiles:        files,
	}
	if !receiptMatchesBinary(receipt, runtimeDigest, modelDigest, files, runner) {
		t.Fatal("valid receipt did not match")
	}
	for _, mutate := range []func(*Receipt){
		func(value *Receipt) { value.ArtifactIdentity = "oci://attacker.invalid/model@sha256:" + modelDigest },
		func(value *Receipt) { value.ModelRepository = "attacker/model" },
	} {
		changed := receipt
		mutate(&changed)
		if receiptMatchesBinary(changed, runtimeDigest, modelDigest, files, runner) {
			t.Fatal("modified provenance matched the binary")
		}
	}
}

func TestVerifyWheelInstallationDetectsModuleTampering(t *testing.T) {
	sitePackages := t.TempDir()
	modulePath := filepath.Join(sitePackages, "example", "module.py")
	if err := os.MkdirAll(filepath.Dir(modulePath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("value = 1\n")
	if err := os.WriteFile(modulePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	wheelPath := filepath.Join(t.TempDir(), "example.whl")
	buffer := new(bytes.Buffer)
	wheel := zip.NewWriter(buffer)
	record, err := wheel.Create("example-1.0.dist-info/RECORD")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	rows := csv.NewWriter(record)
	if err := rows.Write([]string{"example/module.py", "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:]), "10"}); err != nil {
		t.Fatal(err)
	}
	rows.Flush()
	if err := rows.Error(); err != nil {
		t.Fatal(err)
	}
	if err := wheel.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wheelPath, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := zip.OpenReader(wheelPath)
	if err != nil {
		t.Fatal(err)
	}
	allowed := make(map[string]struct{})
	if err := verifyWheelInstallation(sitePackages, opened.File, allowed); err != nil {
		t.Fatal(err)
	}
	opened.Close()
	if err := os.WriteFile(modulePath, []byte("value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err = zip.OpenReader(wheelPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := verifyWheelInstallation(sitePackages, opened.File, make(map[string]struct{})); err == nil {
		t.Fatal("modified Python module passed wheel verification")
	}
}

func TestGeneratedRuntimeFileDoesNotAllowBytecode(t *testing.T) {
	if generatedRuntimeFile("example/__pycache__/module.cpython-312.pyc") {
		t.Fatal("unverified Python bytecode was treated as generated metadata")
	}
}
