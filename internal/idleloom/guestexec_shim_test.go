//go:build darwin || linux

package idleloom

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWSLShim puts an executable named like the WSL launcher on PATH that
// behaves the way wsl.exe does for the arguments wslExec builds: it strips
// "-d <distro> -u <user> --exec" and runs the rest, passing standard input
// through. It lets the WSL executor be exercised for real off Windows, so the
// argument layout and the stdin plumbing are checked by running them rather
// than by comparing strings.
func newWSLShim(t *testing.T) (logPath string) {
	t.Helper()
	directory := t.TempDir()
	logPath = filepath.Join(directory, "invocations.log")
	shim := `#!/bin/sh
printf '%s\n' "$*" >> ` + logPath + `
while [ $# -gt 0 ]; do
	case "$1" in
	-d|-u) shift 2 ;;
	--exec) shift; break ;;
	*) break ;;
	esac
done
exec "$@"
`
	path := filepath.Join(directory, wslBinary)
	if err := os.WriteFile(path, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestWSLExecRunsCommandsThroughTheLauncher(t *testing.T) {
	logPath := newWSLShim(t)
	exec := wslExec{Runner: ExecRunner{}, Distribution: "Ubuntu-24.04"}
	var out bytes.Buffer
	if err := exec.Run(context.Background(), &out, &out, "printf", "%s", "reached-the-guest"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != "reached-the-guest" {
		t.Errorf("command output = %q", out.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "-d Ubuntu-24.04 -u root --exec printf") {
		t.Errorf("launcher was not addressed with the distribution and root: %s", log)
	}
}

// TestWSLExecScriptRunsAMultiLineScript is the case Windows command-line
// quoting would break if the script travelled as an argument.
func TestWSLExecScriptRunsAMultiLineScript(t *testing.T) {
	newWSLShim(t)
	directory := t.TempDir()
	marker := filepath.Join(directory, "marker with spaces")
	exec := wslExec{Runner: ExecRunner{}, Distribution: "Ubuntu"}
	script := "set -eu\nvalue='quoted \"inner\" value'\nprintf '%s' \"$value\" > " + shellQuote(marker) + "\n"
	if err := exec.Script(context.Background(), os.Stderr, os.Stderr, script); err != nil {
		t.Fatalf("Script: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("script did not run: %v", err)
	}
	if string(data) != `quoted "inner" value` {
		t.Errorf("script wrote %q", data)
	}
}

// TestWSLExecSendDeliversBytesVerbatim proves the bundle survives the trip.
// A tar starts with bytes a shell would happily try to execute, which is what
// made an earlier revision of Send corrupt it.
func TestWSLExecSendDeliversBytesVerbatim(t *testing.T) {
	newWSLShim(t)
	directory := t.TempDir()
	source := filepath.Join(directory, "bundle.tar")
	payload := []byte("#!/bin/sh\nrm -rf /should-never-run\n\x00\x01\x02binary\xff")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "nested", "delivered.tar")
	exec := wslExec{Runner: ExecRunner{}, Distribution: "Ubuntu"}
	if err := exec.Send(context.Background(), source, destination); err != nil {
		t.Fatalf("Send: %v", err)
	}
	delivered, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("file was not delivered: %v", err)
	}
	if !bytes.Equal(delivered, payload) {
		t.Errorf("delivered %q, want %q", delivered, payload)
	}
	if _, err := os.Stat("/should-never-run"); err == nil {
		t.Fatal("the payload was executed instead of copied")
	}
}

// TestLinuxExecSendLeavesExistingDirectoryPermissionsAlone is a regression
// test for a real incident: Send created the destination's parent with
// "install -d -m 0700", which also re-permissioned parents that already
// existed. Copying the bundle to /tmp/idleloom-bundle.tar turned /tmp into a
// root-owned 0700 directory on the host and broke every other program on it.
func TestLinuxExecSendLeavesExistingDirectoryPermissionsAlone(t *testing.T) {
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o1777); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(source, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	exec := linuxExec{Runner: ExecRunner{}, Elevate: false}
	destination := filepath.Join(shared, "idleloom-bundle.tar")
	if err := exec.Send(context.Background(), source, destination); err != nil {
		t.Fatalf("Send: %v", err)
	}

	after, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("shared directory mode changed from %v to %v", before.Mode(), after.Mode())
	}
	delivered, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("file was not delivered: %v", err)
	}
	if string(delivered) != "payload" {
		t.Errorf("delivered %q", delivered)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	// The bundle carries a bootstrap token, so it must not be world-readable.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("delivered file mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestLinuxExecSendCreatesMissingParents keeps the other half of the contract:
// a destination whose directory does not exist yet still has to work.
func TestLinuxExecSendCreatesMissingParents(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(source, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "missing", "nested", "bundle.tar")
	exec := linuxExec{Runner: ExecRunner{}, Elevate: false}
	if err := exec.Send(context.Background(), source, destination); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("file was not delivered into a new directory: %v", err)
	}
}
