package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"sigs.k8s.io/yaml"
)

const inspectRevision = "fedcba9876543210fedcba9876543210fedcba98"

type inspectFile struct {
	content []byte
	lfs     bool
	// listedSize overrides the size the tree API reports.
	listedSize int64
}

func inspectServer(t *testing.T, files map[string]inspectFile) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	downloads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/owner/model/tree/"+inspectRevision && r.URL.Query().Get("recursive") == "true":
			var entries []map[string]any
			for name, file := range files {
				size := int64(len(file.content))
				if file.listedSize != 0 {
					size = file.listedSize
				}
				blob := sha1.New()
				_, _ = fmt.Fprintf(blob, "blob %d\x00", len(file.content))
				_, _ = blob.Write(file.content)
				entry := map[string]any{"type": "file", "path": name, "size": size, "oid": hex.EncodeToString(blob.Sum(nil))}
				if file.lfs {
					sum := sha256.Sum256(file.content)
					entry["lfs"] = map[string]any{"oid": hex.EncodeToString(sum[:]), "size": size, "pointerSize": 134}
				}
				entries = append(entries, entry)
			}
			_ = json.NewEncoder(w).Encode(entries)
		case strings.HasPrefix(r.URL.Path, "/owner/model/resolve/"+inspectRevision+"/"):
			name := strings.TrimPrefix(r.URL.Path, "/owner/model/resolve/"+inspectRevision+"/")
			file, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			mu.Lock()
			downloads[name]++
			mu.Unlock()
			_, _ = w.Write(file.content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return downloads[name]
	}
}

func TestModelInspectPrintsReadyToApplyModel(t *testing.T) {
	files := map[string]inspectFile{
		"config.json":       {content: []byte(`{"model_type":"lfm2_moe"}`)},
		"tokenizer.json":    {content: []byte(`{"version":"1.0"}`)},
		"model.safetensors": {content: []byte(strings.Repeat("weights", 128)), lfs: true},
		".gitattributes":    {content: []byte("*.safetensors filter=lfs")},
	}
	server, downloads := inspectServer(t, files)
	var output bytes.Buffer
	err := runModelInspect(context.Background(), []string{
		"hf", "owner/model@" + inspectRevision, "--name", "lfm2-8b-mlx", "--max-context", "8192", "--endpoint", server.URL,
	}, &output, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var model nativev1alpha1.IdleloomModel
	if err := yaml.UnmarshalStrict(output.Bytes(), &model); err != nil {
		t.Fatalf("output is not an IdleloomModel: %v\n%s", err, output.String())
	}
	if model.APIVersion != nativev1alpha1.GroupVersion.String() || model.Kind != "IdleloomModel" || model.Name != "lfm2-8b-mlx" {
		t.Fatalf("object identity = %s %s %s", model.APIVersion, model.Kind, model.Name)
	}
	if err := nativev1alpha1.ValidateModel(&model); err != nil {
		t.Fatalf("printed model does not validate: %v", err)
	}
	var manifest []devruntime.ModelFile
	var size int64
	for name, file := range files {
		if strings.HasPrefix(name, ".") {
			continue
		}
		sum := sha256.Sum256(file.content)
		manifest = append(manifest, devruntime.ModelFile{Path: name, Size: int64(len(file.content)), SHA256: hex.EncodeToString(sum[:])})
		size += int64(len(file.content))
	}
	digest, err := devruntime.MLXManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := model.Spec.Artifact
	if artifact.ManifestDigest != digest || artifact.SizeBytes != size || artifact.HuggingFace == nil ||
		artifact.HuggingFace.Repository != "owner/model" || artifact.HuggingFace.Revision != inspectRevision {
		t.Fatalf("artifact = %#v, want digest %s and size %d", artifact, digest, size)
	}
	if model.Spec.MaxContextLength != 8192 || model.Spec.MinimumUnifiedMemory.Cmp(nativev1alpha1.MinimumUnifiedMemoryForModel(size, 8192)) != 0 {
		t.Fatalf("admission fields = %d %s", model.Spec.MaxContextLength, model.Spec.MinimumUnifiedMemory.String())
	}
	if strings.Contains(output.String(), "status") || strings.Contains(output.String(), "creationTimestamp") {
		t.Fatalf("output carries server-owned fields:\n%s", output.String())
	}
	if downloads("model.safetensors") != 0 || downloads("config.json") != 1 {
		t.Fatalf("downloads: weights=%d config=%d, want only small non-LFS files", downloads("model.safetensors"), downloads("config.json"))
	}

	output.Reset()
	if err := runModelInspect(context.Background(), []string{
		"hf", "owner/model@" + inspectRevision, "--name", "lfm2-8b-mlx", "--min-memory", "12Gi", "--endpoint", server.URL,
	}, &output, server.Client()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "minimumUnifiedMemory: 12Gi") {
		t.Fatalf("explicit memory was not used:\n%s", output.String())
	}
}

func TestModelInspectRefusesUnhashableAndUnpinnedSources(t *testing.T) {
	server, _ := inspectServer(t, map[string]inspectFile{
		"config.json":  {content: []byte(`{}`)},
		"weights.npz":  {content: []byte("x"), listedSize: devruntime.MaxInlineHashBytes + 1},
		"tokenizer.md": {content: []byte("tokenizer")},
	})
	var output bytes.Buffer
	err := runModelInspect(context.Background(), []string{"hf", "owner/model@" + inspectRevision, "--name", "big", "--endpoint", server.URL}, &output, server.Client())
	if err == nil || !strings.Contains(err.Error(), "weights.npz") || !strings.Contains(err.Error(), "not stored with LFS") {
		t.Fatalf("err = %v, want a clear refusal naming the file", err)
	}
	for _, args := range [][]string{
		{"hf", "owner/model@main", "--name", "branch"},
		{"hf", "owner/model", "--name", "missing-revision"},
		{"hf", "owner/model@" + inspectRevision},
		{"hf", "owner/model@" + inspectRevision, "--name", "Not_A_Name"},
		{"oci", "owner/model@" + inspectRevision, "--name", "wrong-source"},
	} {
		if err := runModelInspect(context.Background(), append(args, "--endpoint", server.URL), &output, server.Client()); err == nil {
			t.Fatalf("model inspect accepted %v", args)
		}
	}
}
