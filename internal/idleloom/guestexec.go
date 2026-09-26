package idleloom

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// guestExec runs privileged commands inside the Linux environment that hosts
// the worker kubelet.
//
// The krunkit backend reaches its guest over SSH. The in-place backends do
// not: a Linux host runs the commands directly, and a Windows host runs them
// through wsl.exe. Both are already root-capable without a network hop, so the
// whole SSH key, known-hosts, and port-forward apparatus disappears.
type guestExec interface {
	// Describe names the environment for error messages, e.g. "this host" or
	// `WSL distribution "Ubuntu-24.04"`.
	Describe() string
	// Run executes argv as root and streams its output.
	Run(ctx context.Context, stdout, stderr io.Writer, argv ...string) error
	// Script runs a shell script as root.
	Script(ctx context.Context, stdout, stderr io.Writer, script string) error
	// Send copies a local file to guestPath, owned by root and mode 0600.
	Send(ctx context.Context, localPath, guestPath string) error
}

// shellQuote renders a value safe to embed in a POSIX shell command.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// linuxExec runs commands directly on the Linux host idlectl is running on.
type linuxExec struct {
	Runner CommandRunner
	// Elevate prefixes commands with sudo. It is false when idlectl already
	// runs as root, which is the common case for a service-managed worker.
	Elevate bool
}

func newLinuxExec(runner CommandRunner) linuxExec {
	return linuxExec{Runner: runner, Elevate: os.Geteuid() != 0}
}

func (l linuxExec) Describe() string { return "this host" }

// checkElevation reports why commands cannot be run as root, if they cannot.
// Enrollment installs system services, so a host that offers neither root nor
// a non-interactive sudo has to say so before anything else is attempted.
func (l linuxExec) checkElevation() error {
	if !l.Elevate {
		return nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("idlectl is not running as root and sudo was not found in PATH; enrolling a worker installs system services, so run idlectl with sudo or as root")
	}
	return nil
}

func (l linuxExec) command(argv []string) (string, []string) {
	if l.Elevate {
		return "sudo", append([]string{"-n", "--"}, argv...)
	}
	return argv[0], argv[1:]
}

func (l linuxExec) Run(ctx context.Context, stdout, stderr io.Writer, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command given")
	}
	if err := l.checkElevation(); err != nil {
		return err
	}
	name, args := l.command(argv)
	return l.Runner.Run(ctx, stdout, stderr, name, args...)
}

func (l linuxExec) Script(ctx context.Context, stdout, stderr io.Writer, script string) error {
	return l.Run(ctx, stdout, stderr, "/bin/sh", "-c", script)
}

func (l linuxExec) Send(ctx context.Context, localPath, guestPath string) error {
	// install(1) creates the parent, sets the mode, and replaces the target in
	// one step, so a partially written file is never left behind.
	if err := l.Run(ctx, io.Discard, io.Discard, "install", "-d", "-m", "0700", filepath.Dir(guestPath)); err != nil {
		return fmt.Errorf("create %s on %s: %w", filepath.Dir(guestPath), l.Describe(), err)
	}
	if err := l.Run(ctx, io.Discard, io.Discard, "install", "-m", "0600", localPath, guestPath); err != nil {
		return fmt.Errorf("copy %s to %s on %s: %w", localPath, guestPath, l.Describe(), err)
	}
	return nil
}

// inputRunner extends CommandRunner with standard input. The in-place
// backends stream file contents and scripts into the guest rather than
// staging them on its filesystem first.
type inputRunner interface {
	CommandRunner
	RunWithInput(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// wslExec runs commands inside a WSL2 distribution from a Windows host.
//
// wsl.exe is invoked with --exec, which hands the remaining arguments to the
// binary without a shell in between, so guest paths never pass through cmd.exe
// quoting. Scripts and file contents arrive on standard input for the same
// reason.
type wslExec struct {
	Runner       inputRunner
	Distribution string
}

func (w wslExec) Describe() string {
	return fmt.Sprintf("WSL distribution %q", w.Distribution)
}

func (w wslExec) args(argv []string) []string {
	return append([]string{"-d", w.Distribution, "-u", "root", "--exec"}, argv...)
}

func (w wslExec) Run(ctx context.Context, stdout, stderr io.Writer, argv ...string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command given")
	}
	return w.Runner.Run(ctx, stdout, stderr, wslBinary, w.args(argv)...)
}

func (w wslExec) Script(ctx context.Context, stdout, stderr io.Writer, script string) error {
	// "sh -s" reads the script from standard input, so no part of it is
	// subject to Windows command-line quoting.
	return w.Runner.RunWithInput(ctx, strings.NewReader(script), stdout, stderr,
		wslBinary, w.args([]string{"/bin/sh", "-s"})...)
}

func (w wslExec) Send(ctx context.Context, localPath, guestPath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer func() { _ = file.Close() }()
	// Standard input carries the file, so the copy script has to travel as an
	// argument instead. umask runs before the redirection creates the file, so
	// the contents are never briefly readable by other users on the guest.
	script := "set -e; umask 077; mkdir -p " + shellQuote(guestDir(guestPath)) +
		"; cat > " + shellQuote(guestPath)
	if err := w.Runner.RunWithInput(ctx, file, io.Discard, io.Discard,
		wslBinary, w.args([]string{"/bin/sh", "-c", script})...); err != nil {
		return fmt.Errorf("copy %s to %s in %s: %w", localPath, guestPath, w.Describe(), err)
	}
	return nil
}

// guestDir is path.Dir for guest paths. filepath.Dir would use the host's
// separator, which is wrong when a Windows host addresses a Linux guest.
func guestDir(guestPath string) string {
	return path.Dir(guestPath)
}

// wslBinary is the Windows Subsystem for Linux launcher. It lives in System32
// and is resolved through PATH so a test can shadow it.
const wslBinary = "wsl.exe"
