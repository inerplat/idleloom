package devruntime

import (
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
