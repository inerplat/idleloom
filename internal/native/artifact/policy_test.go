package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
)

func TestPolicyAcceptsRestrictedSafetensorsManifest(t *testing.T) {
	manifest := validManifest()
	if err := DefaultPolicy().ValidateDeclaration(manifest); err != nil {
		t.Fatalf("ValidateDeclaration: %v", err)
	}
}

func TestPolicyRejectsUnsafeFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "python", mutate: func(m *Manifest) { m.Files[0].Path = "model.py" }},
		{name: "pickle", mutate: func(m *Manifest) { m.Files[0].Path = "model.pkl" }},
		{name: "symlink", mutate: func(m *Manifest) { m.Files[0].Type = FileTypeSymlink }},
		{name: "hardlink", mutate: func(m *Manifest) { m.Files[0].Type = FileTypeHardlink }},
		{name: "executable", mutate: func(m *Manifest) { m.Files[0].Mode = 0o755 }},
		{name: "traversal", mutate: func(m *Manifest) { m.Files[0].Path = "../model.safetensors" }},
		{name: "absolute", mutate: func(m *Manifest) { m.Files[0].Path = "/tmp/model.safetensors" }},
		{name: "archive", mutate: func(m *Manifest) { m.Files[0].Path = "model.tar" }},
		{name: "uppercase", mutate: func(m *Manifest) { m.Files[0].Path = "Model.safetensors" }},
		{name: "duplicate", mutate: func(m *Manifest) { m.Files[1].Path = m.Files[0].Path }},
		{name: "total-mismatch", mutate: func(m *Manifest) { m.TotalSizeBytes++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifest()
			test.mutate(&manifest)
			if err := DefaultPolicy().ValidateDeclaration(manifest); err == nil {
				t.Fatal("ValidateDeclaration accepted an unsafe artifact manifest")
			}
		})
	}
}

func TestPolicyPinsManifestToCatalog(t *testing.T) {
	manifest := validManifest()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	digest := sha256.Sum256(data)
	catalog := nativev1alpha1.ModelArtifact{ManifestDigest: "sha256:" + hex.EncodeToString(digest[:]), SizeBytes: manifest.TotalSizeBytes}
	if _, err := DefaultPolicy().VerifyManifestBlob(catalog, data); err != nil {
		t.Fatalf("VerifyManifestBlob: %v", err)
	}
	catalog.ManifestDigest = testDigest("e")
	if _, err := DefaultPolicy().VerifyManifestBlob(catalog, data); err == nil {
		t.Fatal("VerifyManifestBlob accepted bytes with the wrong catalog digest")
	}
}

func TestPolicyVerifiesActualExtractedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod staging root: %v", err)
	}
	contents := map[string][]byte{
		"model.safetensors": []byte("weights"),
		"config.json":       []byte(`{"model_type":"qwen3_5"}`),
		"tokenizer.json":    []byte(`{"version":"1.0"}`),
	}
	manifest := Manifest{SchemaVersion: SchemaV1Alpha1}
	for _, name := range []string{"model.safetensors", "config.json", "tokenizer.json"} {
		data := contents[name]
		digest := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, File{Path: name, Type: FileTypeRegular, SizeBytes: int64(len(data)), SHA256: "sha256:" + hex.EncodeToString(digest[:]), Mode: 0o600})
		manifest.TotalSizeBytes += int64(len(data))
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if err := DefaultPolicy().VerifyExtractedTree(root, manifest); err != nil {
		t.Fatalf("VerifyExtractedTree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile extra: %v", err)
	}
	if err := DefaultPolicy().VerifyExtractedTree(root, manifest); err == nil {
		t.Fatal("VerifyExtractedTree accepted an undeclared file")
	}
	if err := os.Remove(filepath.Join(root, "extra.json")); err != nil {
		t.Fatalf("Remove extra: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "model.safetensors")); err != nil {
		t.Fatalf("Remove weights: %v", err)
	}
	if err := os.Symlink("config.json", filepath.Join(root, "model.safetensors")); err != nil {
		t.Fatalf("Symlink weights: %v", err)
	}
	if err := DefaultPolicy().VerifyExtractedTree(root, manifest); err == nil {
		t.Fatal("VerifyExtractedTree followed a symlink")
	}
	if err := os.Remove(filepath.Join(root, "model.safetensors")); err != nil {
		t.Fatalf("Remove symlink: %v", err)
	}
	external := filepath.Join(t.TempDir(), "weights")
	if err := os.WriteFile(external, contents["model.safetensors"], 0o600); err != nil {
		t.Fatalf("WriteFile external weights: %v", err)
	}
	if err := os.Link(external, filepath.Join(root, "model.safetensors")); err != nil {
		t.Fatalf("Link weights: %v", err)
	}
	if err := DefaultPolicy().VerifyExtractedTree(root, manifest); err == nil {
		t.Fatal("VerifyExtractedTree accepted a hard-linked file")
	}
}

func validManifest() Manifest {
	return Manifest{
		SchemaVersion:  SchemaV1Alpha1,
		TotalSizeBytes: 4096,
		Files: []File{
			{Path: "model.safetensors", Type: FileTypeRegular, SizeBytes: 2048, SHA256: testDigest("b"), Mode: 0o644},
			{Path: "config.json", Type: FileTypeRegular, SizeBytes: 1024, SHA256: testDigest("c"), Mode: 0o644},
			{Path: "tokenizer.json", Type: FileTypeRegular, SizeBytes: 1024, SHA256: testDigest("d"), Mode: 0o644},
		},
	}
}

func testDigest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
