package idleloom

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturingRunner records the process invocations a guest executor builds,
// including anything it streams on standard input.
type capturingRunner struct {
	calls  [][]string
	inputs []string
	reply  string
}

func (c *capturingRunner) Run(_ context.Context, stdout, _ io.Writer, name string, args ...string) error {
	c.calls = append(c.calls, append([]string{name}, args...))
	c.inputs = append(c.inputs, "")
	if c.reply != "" && stdout != nil {
		_, _ = io.WriteString(stdout, c.reply)
	}
	return nil
}

func (c *capturingRunner) RunWithInput(_ context.Context, stdin io.Reader, _, _ io.Writer, name string, args ...string) error {
	c.calls = append(c.calls, append([]string{name}, args...))
	data, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	c.inputs = append(c.inputs, string(data))
	return nil
}

func (c *capturingRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

func TestWSLExecTargetsTheNamedDistributionAsRoot(t *testing.T) {
	runner := &capturingRunner{}
	exec := wslExec{Runner: runner, Distribution: "Ubuntu-24.04"}
	if err := exec.Run(context.Background(), io.Discard, io.Discard, "systemctl", "is-active", "kubelet.service"); err != nil {
		t.Fatal(err)
	}
	want := []string{wslBinary, "-d", "Ubuntu-24.04", "-u", "root", "--exec", "systemctl", "is-active", "kubelet.service"}
	if strings.Join(runner.calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("wsl invocation = %v, want %v", runner.calls[0], want)
	}
}

// TestWSLExecScriptTravelsOnStandardInput matters because a Windows host
// builds one command-line string: a multi-line script passed as an argument
// would be re-split by the receiving process's own parsing rules.
func TestWSLExecScriptTravelsOnStandardInput(t *testing.T) {
	runner := &capturingRunner{}
	exec := wslExec{Runner: runner, Distribution: "Ubuntu"}
	script := "set -eu\necho 'a b'  \"c\"\n"
	if err := exec.Script(context.Background(), io.Discard, io.Discard, script); err != nil {
		t.Fatal(err)
	}
	call := strings.Join(runner.calls[0], " ")
	if !strings.HasSuffix(call, "--exec /bin/sh -s") {
		t.Errorf("script is not read from standard input: %s", call)
	}
	if runner.inputs[0] != script {
		t.Errorf("script on stdin = %q, want %q", runner.inputs[0], script)
	}
}

// TestWSLExecSendStreamsTheFileNotTheScript pins the split that a previous
// revision got wrong: standard input carries the file, so the copy script has
// to be an argument. Passing both on stdin made sh execute the file.
func TestWSLExecSendStreamsTheFileNotTheScript(t *testing.T) {
	local := filepath.Join(t.TempDir(), "bundle.tar")
	contents := "tar-bytes-not-a-script\n"
	if err := os.WriteFile(local, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &capturingRunner{}
	exec := wslExec{Runner: runner, Distribution: "Ubuntu"}
	if err := exec.Send(context.Background(), local, "/tmp/idleloom-bundle.tar"); err != nil {
		t.Fatal(err)
	}
	if runner.inputs[0] != contents {
		t.Errorf("stdin carried %q, want the file contents %q", runner.inputs[0], contents)
	}
	call := runner.calls[0]
	script := call[len(call)-1]
	if !strings.Contains(script, "cat > '/tmp/idleloom-bundle.tar'") {
		t.Errorf("copy script does not redirect into the guest path: %s", script)
	}
	if !strings.Contains(script, "umask 077") {
		t.Error("copy script does not restrict the file mode before creating it")
	}
	if !strings.Contains(script, "mkdir -p '/tmp'") {
		t.Errorf("copy script does not create the parent directory: %s", script)
	}
}

// TestGuestDirUsesForwardSlashes guards the Windows-to-Linux direction:
// filepath.Dir on a Windows host would answer with backslash semantics for a
// path that is going to be interpreted by a Linux shell.
func TestGuestDirUsesForwardSlashes(t *testing.T) {
	if got := guestDir("/var/lib/idleloom/config/install.sh"); got != "/var/lib/idleloom/config" {
		t.Errorf("guestDir = %q, want /var/lib/idleloom/config", got)
	}
}

func TestShellQuoteNeutralisesEmbeddedQuotes(t *testing.T) {
	if got := shellQuote(`a'b`); got != `'a'\''b'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// TestWSLExecOmitsTheDistributionWhenUnset covers the default case: the flag
// is optional, and wsl.exe expresses "use the default distribution" by having
// no -d at all. Passing -d "" asks for a distribution named the empty string.
func TestWSLExecOmitsTheDistributionWhenUnset(t *testing.T) {
	runner := &capturingRunner{}
	exec := wslExec{Runner: runner}
	if err := exec.Run(context.Background(), io.Discard, io.Discard, "uname", "-s"); err != nil {
		t.Fatal(err)
	}
	call := runner.calls[0]
	for index, arg := range call {
		if arg == "-d" {
			t.Fatalf("wsl invocation carries -d with no distribution: %v", call)
		}
		if arg == "" {
			t.Fatalf("wsl invocation has an empty argument at %d: %v", index, call)
		}
	}
	if strings.Join(call, " ") != wslBinary+" -u root --exec uname -s" {
		t.Errorf("wsl invocation = %v", call)
	}
	if exec.Describe() == `WSL distribution ""` {
		t.Error("an unset distribution is described as an empty name")
	}
}
