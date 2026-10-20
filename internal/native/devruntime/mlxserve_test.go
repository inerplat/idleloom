package devruntime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMLXServeArgsSetDefaultOutputLimit(t *testing.T) {
	args, err := mlxServeArgs("/root/runtime/server.sb", "/root/venv/bin/python", "/root/models/m", "127.0.0.1", "41000", 131072)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-f", "/root/runtime/server.sb", "/root/venv/bin/python", "-I", "-B",
		"-m", "mlx_lm.server", "--model", "/root/models/m", "--host", "127.0.0.1", "--port", "41000",
		"--max-tokens", "131072",
		"--decode-concurrency", "1", "--prompt-concurrency", "1", "--prompt-cache-size", "1"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestMLXServeArgsRejectMissingOutputLimit(t *testing.T) {
	for _, maxTokens := range []int{0, -1} {
		if _, err := mlxServeArgs("p", "py", "m", "127.0.0.1", "1", maxTokens); err == nil {
			t.Fatalf("maxTokens %d accepted; the server would fall back to its 512-token default", maxTokens)
		}
	}
}

func TestMLXServeModelArgumentLinksTheAlias(t *testing.T) {
	work := t.TempDir()
	model := t.TempDir()
	dir, arg, err := mlxServeModelArgument(work, model, "lfm2.5-24b-a2b")
	if err != nil {
		t.Fatal(err)
	}
	if arg != "lfm2.5-24b-a2b" || filepath.Dir(dir) != work {
		t.Fatalf("serve directory %q and model argument %q", dir, arg)
	}
	target, err := os.Readlink(filepath.Join(dir, arg))
	if err != nil || target != model {
		t.Fatalf("alias link points at %q (%v), want %q", target, err, model)
	}
	// A restart replaces the previous link instead of failing on it.
	if _, _, err := mlxServeModelArgument(work, model, "lfm2.5-24b-a2b"); err != nil {
		t.Fatal(err)
	}
}

func TestMLXServeModelArgumentWithoutAliasUsesTheDirectory(t *testing.T) {
	work := t.TempDir()
	dir, arg, err := mlxServeModelArgument(work, "/models/m", "")
	if err != nil {
		t.Fatal(err)
	}
	if dir != work || arg != "/models/m" {
		t.Fatalf("serve directory %q and model argument %q", dir, arg)
	}
}

func TestMLXServeModelArgumentRejectsPathAliases(t *testing.T) {
	for _, alias := range []string{".", "..", "a/b", "../x"} {
		if _, _, err := mlxServeModelArgument(t.TempDir(), "/models/m", alias); err == nil {
			t.Fatalf("alias %q was accepted", alias)
		}
	}
}

func TestMLXServeEnvProvidesAnEmptyHuggingFaceCache(t *testing.T) {
	serve := t.TempDir()
	env, err := mlxServeEnv("/work", serve)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(serve, "hf-cache")
	if !slices.Contains(env, "HF_HUB_CACHE="+cache) || !slices.Contains(env, "HF_HUB_OFFLINE=1") {
		t.Fatalf("env = %q", env)
	}
	if info, err := os.Stat(cache); err != nil || !info.IsDir() {
		t.Fatalf("cache directory: %v", err)
	}
}
