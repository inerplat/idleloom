package devruntime

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

type fakeHFFile struct {
	content []byte
	lfs     bool
	// served replaces the bytes the resolve endpoint returns.
	served []byte
}

type fakeHF struct {
	repository string
	files      map[string]fakeHFFile
	pageSize   int
	mu         sync.Mutex
	downloads  map[string]int
}

func newFakeHF(t *testing.T, files map[string]fakeHFFile) (*fakeHF, *httptest.Server) {
	fake := &fakeHF{repository: "owner/model", files: files, downloads: make(map[string]int)}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeHF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	treePrefix := "/api/models/" + f.repository + "/tree/" + testRevision
	resolvePrefix := "/" + f.repository + "/resolve/" + testRevision + "/"
	switch {
	case r.URL.Path == treePrefix:
		if r.URL.Query().Get("recursive") != "true" {
			http.Error(w, "recursive listing required", http.StatusBadRequest)
			return
		}
		var entries []map[string]any
		directories := map[string]bool{}
		for name, file := range f.files {
			for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
				directories[parent] = true
			}
			blob := sha1.New()
			_, _ = fmt.Fprintf(blob, "blob %d\x00", len(file.content))
			_, _ = blob.Write(file.content)
			entry := map[string]any{"type": "file", "path": name, "size": len(file.content), "oid": hex.EncodeToString(blob.Sum(nil))}
			if file.lfs {
				sum := sha256.Sum256(file.content)
				entry["oid"] = strings.Repeat("e", 40)
				entry["lfs"] = map[string]any{"oid": hex.EncodeToString(sum[:]), "size": len(file.content), "pointerSize": 134}
			}
			entries = append(entries, entry)
		}
		for name := range directories {
			entries = append(entries, map[string]any{"type": "directory", "path": name, "size": 0, "oid": strings.Repeat("d", 40)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i]["path"].(string) < entries[j]["path"].(string) })
		start := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			_, _ = fmt.Sscanf(cursor, "%d", &start)
		}
		end := len(entries)
		if f.pageSize > 0 && start+f.pageSize < end {
			end = start + f.pageSize
			w.Header().Set("Link", fmt.Sprintf(`<%s?recursive=true&cursor=%d>; rel="next"`, treePrefix, end))
		}
		_ = json.NewEncoder(w).Encode(entries[start:end])
	case strings.HasPrefix(r.URL.Path, resolvePrefix):
		name := strings.TrimPrefix(r.URL.Path, resolvePrefix)
		file, ok := f.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.downloads[name]++
		f.mu.Unlock()
		if file.served != nil {
			_, _ = w.Write(file.served)
			return
		}
		_, _ = w.Write(file.content)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeHF) downloadCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads[name]
}

func fakeModelFiles() map[string]fakeHFFile {
	return map[string]fakeHFFile{
		"config.json":                  {content: []byte(`{"model_type":"lfm2_moe"}`)},
		"tokenizer.json":               {content: []byte(`{"version":"1.0"}`)},
		"model.safetensors.index.json": {content: []byte(`{"weight_map":{}}`)},
		"model-00001-of-00002.safetensors": {
			content: []byte(strings.Repeat("weights-one", 64)), lfs: true,
		},
		"model-00002-of-00002.safetensors": {
			content: []byte(strings.Repeat("weights-two", 64)), lfs: true,
		},
		"chat/template.jinja": {content: []byte("{{ messages }}")},
		"100%.txt":            {content: []byte("percent names are escaped")},
		".gitattributes":      {content: []byte("*.safetensors filter=lfs")},
		"chat/.cache":         {content: []byte("ignored")},
	}
}

func expectedModel(t *testing.T, files map[string]fakeHFFile) HFModel {
	t.Helper()
	var manifest []ModelFile
	var size int64
	for name, file := range files {
		if !inMLXManifest(name) {
			continue
		}
		sum := sha256.Sum256(file.content)
		manifest = append(manifest, ModelFile{Path: name, Size: int64(len(file.content)), SHA256: hex.EncodeToString(sum[:])})
		size += int64(len(file.content))
	}
	digest, err := MLXManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return HFModel{Repository: "owner/model", Revision: testRevision, ManifestDigest: digest, SizeBytes: size}
}

func TestMLXManifestDigestIsDeterministicAndDocumented(t *testing.T) {
	one := strings.Repeat("1", 64)
	two := strings.Repeat("2", 64)
	files := []ModelFile{
		{Path: "tokenizer.json", Size: 20, SHA256: two},
		{Path: "config.json", Size: 10, SHA256: one},
	}
	reversed := []ModelFile{files[1], files[0]}
	first, err := MLXManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MLXManifestDigest(reversed)
	if err != nil {
		t.Fatal(err)
	}
	text := "config.json\t10\t" + one + "\ntokenizer.json\t20\t" + two + "\n"
	sum := sha256.Sum256([]byte(text))
	if want := "sha256:" + hex.EncodeToString(sum[:]); first != want || second != want {
		t.Fatalf("digests = %s, %s; want %s", first, second, want)
	}
	if files[0].Path != "tokenizer.json" {
		t.Fatal("MLXManifestDigest reordered its input")
	}
	changed := []ModelFile{files[0], {Path: "config.json", Size: 11, SHA256: one}}
	if other, _ := MLXManifestDigest(changed); other == first {
		t.Fatal("a size change did not change the manifest digest")
	}
	for name, invalid := range map[string][]ModelFile{
		"dotfile":   {{Path: ".gitattributes", Size: 1, SHA256: one}},
		"duplicate": {{Path: "a.json", Size: 1, SHA256: one}, {Path: "a.json", Size: 1, SHA256: one}},
		"case":      {{Path: "A.json", Size: 1, SHA256: one}, {Path: "a.json", Size: 1, SHA256: one}},
		"dir case":  {{Path: "Sub/a.json", Size: 1, SHA256: one}, {Path: "sub/b.json", Size: 1, SHA256: one}},
		"file dir":  {{Path: "sub", Size: 1, SHA256: one}, {Path: "sub/b.json", Size: 1, SHA256: one}},
		"traversal": {{Path: "../a.json", Size: 1, SHA256: one}},
		"digest":    {{Path: "a.json", Size: 1, SHA256: "abc"}},
		"empty":     nil,
	} {
		if _, err := MLXManifestDigest(invalid); err == nil {
			t.Fatalf("%s manifest was accepted", name)
		}
	}
}

func TestResolveHFManifestExcludesDotfilesAndFollowsPagination(t *testing.T) {
	files := fakeModelFiles()
	fake, server := newFakeHF(t, files)
	fake.pageSize = 3
	resolved, err := ResolveHFManifest(context.Background(), server.Client(), server.URL, "owner/model", testRevision)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range resolved.Files {
		names = append(names, file.Path)
	}
	want := "100%.txt,chat/template.jinja,config.json,model-00001-of-00002.safetensors,model-00002-of-00002.safetensors,model.safetensors.index.json,tokenizer.json"
	if strings.Join(names, ",") != want {
		t.Fatalf("manifest files = %v, want %s", names, want)
	}
	expected := expectedModel(t, files)
	if resolved.Digest != expected.ManifestDigest || resolved.SizeBytes != expected.SizeBytes {
		t.Fatalf("resolved manifest = %s %d bytes, want %s %d bytes", resolved.Digest, resolved.SizeBytes, expected.ManifestDigest, expected.SizeBytes)
	}
	if fake.downloadCount("model-00001-of-00002.safetensors") != 0 || fake.downloadCount(".gitattributes") != 0 {
		t.Fatal("resolving the manifest downloaded LFS weights or dotfiles")
	}
}

func TestResolveHFManifestRefusesLargeFilesWithoutLFS(t *testing.T) {
	_, server := newFakeHF(t, map[string]fakeHFFile{"weights.bin": {content: make([]byte, MaxInlineHashBytes+1)}})
	_, err := ResolveHFManifest(context.Background(), server.Client(), server.URL, "owner/model", testRevision)
	if err == nil || !strings.Contains(err.Error(), "not stored with LFS") {
		t.Fatalf("err = %v, want a clear refusal", err)
	}
}

func TestDownloadHFModelVerifiesAndDiscovers(t *testing.T) {
	files := fakeModelFiles()
	_, server := newFakeHF(t, files)
	layout := NewLayout(t.TempDir())
	model := expectedModel(t, files)
	preparer := Preparer{HTTPClient: server.Client(), HFEndpoint: server.URL}
	directory := HFModelDirectory(layout, model.ManifestDigest)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".download-stale"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := preparer.downloadHFModel(context.Background(), layout, model)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Serves(model) || len(receipt.Sources) != 1 || len(receipt.Files) != 7 {
		t.Fatalf("receipt = %#v", receipt)
	}
	if _, err := os.Stat(filepath.Join(directory, "chat", "template.jinja")); err != nil {
		t.Fatalf("nested model file was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, ".download-stale")); !os.IsNotExist(err) {
		t.Fatal("an unlisted file survived preparation")
	}
	if _, err := verifyHFModel(layout, model, true); err != nil {
		t.Fatalf("strong verification: %v", err)
	}
	discovered := DiscoverHFModels(layout)
	if len(discovered) != 1 || !discovered[0].Serves(model) {
		t.Fatalf("discovered = %#v", discovered)
	}

	weights := filepath.Join(directory, "model-00001-of-00002.safetensors")
	original, err := os.ReadFile(weights)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff
	if err := os.WriteFile(weights, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, false); err != nil {
		t.Fatalf("fast verification should not rehash weights: %v", err)
	}
	if _, err := verifyHFModel(layout, model, true); err == nil {
		t.Fatal("strong verification accepted tampered weights")
	}
	if err := os.WriteFile(weights, original, 0o600); err != nil {
		t.Fatal(err)
	}

	extra := filepath.Join(directory, "model-00003-of-00002.safetensors")
	if err := os.WriteFile(extra, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, false); err == nil {
		t.Fatal("verification accepted a file the manifest does not name")
	}
	if len(DiscoverHFModels(layout)) != 0 {
		t.Fatal("discovery advertised a model directory with a planted file")
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(directory, "config.json")
	outside := filepath.Join(t.TempDir(), "config.json")
	if err := os.Rename(config, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, config); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, false); err == nil {
		t.Fatal("verification followed a symbolic link")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, config); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, false); err == nil {
		t.Fatal("verification accepted a hard-linked file")
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, true); err != nil {
		t.Fatalf("restored model failed verification: %v", err)
	}

	other := model
	other.Revision = strings.Repeat("f", 40)
	if _, err := verifyHFModel(layout, other, false); err == nil {
		t.Fatal("a receipt for one revision verified another")
	}
	if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte(`{"model_file":"x.py"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyHFModel(layout, model, false); err == nil {
		t.Fatal("verification accepted a config that names executable code")
	}
}

func TestDownloadHFModelRecordsEveryCommitWithTheSameManifest(t *testing.T) {
	files := fakeModelFiles()
	_, server := newFakeHF(t, files)
	fork, forkServer := newFakeHF(t, files)
	fork.repository = "fork/model"
	layout := NewLayout(t.TempDir())
	model := expectedModel(t, files)
	if _, err := (Preparer{HTTPClient: server.Client(), HFEndpoint: server.URL}).downloadHFModel(context.Background(), layout, model); err != nil {
		t.Fatal(err)
	}
	forked := model
	forked.Repository = "fork/model"
	receipt, err := (Preparer{HTTPClient: forkServer.Client(), HFEndpoint: forkServer.URL}).downloadHFModel(context.Background(), layout, forked)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Serves(model) || !receipt.Serves(forked) || len(receipt.Models()) != 2 {
		t.Fatalf("receipt sources = %#v", receipt.Sources)
	}
	for _, candidate := range []HFModel{model, forked} {
		if _, err := verifyHFModel(layout, candidate, true); err != nil {
			t.Fatalf("%s: %v", candidate.Repository, err)
		}
	}
	if discovered := DiscoverHFModels(layout); len(discovered) != 1 || len(discovered[0].Models()) != 2 {
		t.Fatalf("discovered = %#v", discovered)
	}
}

func TestResolveHFManifestRefusesConfigThatRunsCode(t *testing.T) {
	for name, config := range map[string]string{
		"top level": `{"model_type":"custom","model_file":"modeling.py"}`,
		"nested":    `{"model_type":"custom","text_config":{"model_file":"/tmp/x.py"}}`,
		"not json":  `model_file`,
	} {
		t.Run(name, func(t *testing.T) {
			_, server := newFakeHF(t, map[string]fakeHFFile{"config.json": {content: []byte(config)}})
			if _, err := ResolveHFManifest(context.Background(), server.Client(), server.URL, "owner/model", testRevision); err == nil {
				t.Fatal("a config that names executable code was accepted")
			}
		})
	}
	_, server := newFakeHF(t, map[string]fakeHFFile{"weights.safetensors": {content: []byte("w"), lfs: true}})
	if _, err := ResolveHFManifest(context.Background(), server.Client(), server.URL, "owner/model", testRevision); err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("err = %v, want a refusal for a repository without config.json", err)
	}
}

func TestDownloadHFModelRefusesMismatchBeforeDownloadingWeights(t *testing.T) {
	files := fakeModelFiles()
	for name, mutate := range map[string]func(*HFModel){
		"digest": func(model *HFModel) { model.ManifestDigest = "sha256:" + strings.Repeat("0", 64) },
		"size":   func(model *HFModel) { model.SizeBytes++ },
	} {
		t.Run(name, func(t *testing.T) {
			fake, server := newFakeHF(t, files)
			layout := NewLayout(t.TempDir())
			model := expectedModel(t, files)
			mutate(&model)
			_, err := (Preparer{HTTPClient: server.Client(), HFEndpoint: server.URL}).downloadHFModel(context.Background(), layout, model)
			if err == nil || !strings.Contains(err.Error(), "pinned") {
				t.Fatalf("err = %v, want a pinned-model refusal", err)
			}
			if fake.downloadCount("model-00001-of-00002.safetensors") != 0 {
				t.Fatal("weights were downloaded for a refused model")
			}
			if _, err := os.Stat(layout.HFModels); !os.IsNotExist(err) {
				t.Fatal("a refused model created storage")
			}
		})
	}
}

func TestDownloadHFModelRejectsServedContentThatDiffersFromLFS(t *testing.T) {
	files := fakeModelFiles()
	weights := files["model-00002-of-00002.safetensors"]
	weights.served = []byte(strings.Repeat("weights-bad", 64))
	files["model-00002-of-00002.safetensors"] = weights
	_, server := newFakeHF(t, files)
	layout := NewLayout(t.TempDir())
	model := expectedModel(t, files)
	_, err := (Preparer{HTTPClient: server.Client(), HFEndpoint: server.URL}).downloadHFModel(context.Background(), layout, model)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v, want a SHA-256 mismatch", err)
	}
	if _, err := os.Stat(hfReceiptPath(layout, model.ManifestDigest)); !os.IsNotExist(err) {
		t.Fatal("a receipt was written for a failed download")
	}
}

func TestListHFFilesRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"../escape.json", "a/../../escape.json", "/absolute.json", `sub\file.json`, "sub//file.json", "./config.json", "sub/:file", "café.json"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": name, "size": 1, "oid": strings.Repeat("a", 40)}})
			}))
			defer server.Close()
			if _, err := listHFFiles(context.Background(), server.Client(), server.URL, "owner/model", testRevision); err == nil {
				t.Fatalf("path %q was accepted", name)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://elsewhere.invalid/api/models/owner/model/tree/x?cursor=1>; rel="next"`)
		_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "config.json", "size": 1, "oid": strings.Repeat("a", 40)}})
	}))
	defer server.Close()
	if _, err := listHFFiles(context.Background(), server.Client(), server.URL, "owner/model", testRevision); err == nil {
		t.Fatal("pagination to another host was followed")
	}
	if _, err := listHFFiles(context.Background(), server.Client(), server.URL, "owner/model", "main"); err == nil {
		t.Fatal("a branch revision was listed")
	}
}

func TestDownloadHFModelReplacesSymlinkedParentDirectory(t *testing.T) {
	files := fakeModelFiles()
	_, server := newFakeHF(t, files)
	layout := NewLayout(t.TempDir())
	model := expectedModel(t, files)
	directory := HFModelDirectory(layout, model.ManifestDigest)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(directory, "chat")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Preparer{HTTPClient: server.Client(), HFEndpoint: server.URL}).downloadHFModel(context.Background(), layout, model); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("download wrote through a symbolic link outside the model directory")
	}
	if info, err := os.Lstat(filepath.Join(directory, "chat")); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("chat is not a plain directory after preparation: %v", err)
	}
	if _, err := verifyHFModel(layout, model, true); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxProfileGrantsOnlyTheModelDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox profile")
	}
	layout := NewLayout(t.TempDir())
	model := HFModelDirectory(layout, "sha256:"+strings.Repeat("a", 64))
	for _, directory := range []string{layout.Work, model} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := sandboxProfile(layout, model, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalPath(model)
	if err != nil {
		t.Fatal(err)
	}
	store, err := canonicalPath(filepath.Join(layout.Root, "models"))
	if err != nil {
		t.Fatal(err)
	}
	deny := strings.Index(profile, `(deny file-read* (subpath "`+store+`"))`)
	allow := strings.Index(profile, `(allow file-read* (subpath "`+canonical+`"))`)
	if deny < 0 || allow < deny {
		t.Fatalf("sandbox profile does not deny the model store and then grant only the model directory:\n%s", profile)
	}
	if strings.Contains(profile, `(allow file-read* (subpath "`+filepath.Dir(canonical)+`"))`) {
		t.Fatal("sandbox profile grants the whole model store")
	}
	if _, err := sandboxProfile(layout, layout.HFModels, nil, ""); err == nil {
		t.Fatal("sandbox profile accepted a model directory that is the whole store")
	}
	if _, err := sandboxProfile(layout, filepath.Join(layout.Root, "models"), nil, ""); err == nil {
		t.Fatal("sandbox profile accepted the model root itself")
	}
	if _, err := sandboxProfile(layout, layout.Work, nil, ""); err == nil {
		t.Fatal("sandbox profile accepted a model directory outside the model root")
	}
	if _, err := sandboxProfile(layout, model, []string{layout.HFModels}, ""); err == nil {
		t.Fatal("sandbox profile accepted a model directory inside a denied path")
	}
}
