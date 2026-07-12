package devruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartShellSupportsSandboxAndHostIsolation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native shell execution requires macOS")
	}
	for _, isolation := range []string{"Sandbox", "Host"} {
		t.Run(isolation, func(t *testing.T) {
			network := "None"
			if isolation == "Host" {
				network = "Outbound"
			}
			var output bytes.Buffer
			process, err := StartShell(context.Background(), ShellConfig{
				Layout: NewLayout(t.TempDir()), Script: "printf 'shell-ready\\n'",
				Isolation: isolation, Network: network, Timeout: 10 * time.Second,
				Output: &output,
			})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for process.Alive() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if process.Alive() {
				t.Fatal("shell process did not complete")
			}
			if err := process.WaitError(); err != nil {
				t.Fatalf("shell process failed: %v; stderr=%s", err, process.Stderr())
			}
			if output.String() != "shell-ready\n" {
				t.Fatalf("output = %q", output.String())
			}
		})
	}
}

func TestHostShellRejectsUnenforceableNetworkIsolation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native shell execution requires macOS")
	}
	_, err := StartShell(context.Background(), ShellConfig{
		Layout: NewLayout(t.TempDir()), Script: "true", Isolation: "Host", Network: "None",
	})
	if err == nil || !strings.Contains(err.Error(), "requires outbound") {
		t.Fatalf("StartShell error = %v", err)
	}
}

func TestHostShellEnvironmentExcludesDaemonSecrets(t *testing.T) {
	t.Setenv("IDLELOOM_TEST_SECRET", "must-not-leak")
	environment := hostShellEnvironment("/tmp/assignment")
	for _, value := range environment {
		if strings.HasPrefix(value, "IDLELOOM_TEST_SECRET=") {
			t.Fatalf("host shell inherited daemon secret: %q", value)
		}
	}
	if !containsEnvironment(environment, "TMPDIR=/tmp/assignment") {
		t.Fatalf("host shell environment = %v", environment)
	}
}

func containsEnvironment(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestShellStartupContextDoesNotOwnProcessLifetime(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native shell execution requires macOS")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	process, err := StartShell(ctx, ShellConfig{
		Layout: NewLayout(t.TempDir()), Script: "sleep 0.1; printf ready",
		Isolation: "Host", Network: "Outbound", Timeout: 10 * time.Second, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for process.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if process.Alive() {
		t.Fatal("shell process did not complete")
	}
	if err := process.WaitError(); err != nil {
		t.Fatalf("shell process was tied to startup context: %v", err)
	}
	if output.String() != "ready" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestShellSandboxProfileRestrictsHomeWritesAndNetwork(t *testing.T) {
	profile, err := shellSandboxProfile(NewLayout(t.TempDir()).Work, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = canonicalPath(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{home, "/Users", "/Volumes", "/Network"} {
		if !strings.Contains(profile, denied) {
			t.Fatalf("sandbox profile does not deny %s: %s", denied, profile)
		}
	}
	if !strings.Contains(profile, "(deny network*)") || !strings.Contains(profile, "(allow file-write* (subpath") {
		t.Fatalf("sandbox profile does not enforce network and write boundaries: %s", profile)
	}
}

func TestSandboxedShellCannotReadExplicitlyDeniedPath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native shell execution requires macOS")
	}
	deniedDirectory := t.TempDir()
	secret := filepath.Join(deniedDirectory, "secret")
	if err := os.WriteFile(secret, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	process, err := StartShell(context.Background(), ShellConfig{
		Layout:    NewLayout(t.TempDir()),
		Script:    "if cat '" + secret + "' >/dev/null 2>&1; then exit 42; else printf 'denied\\n'; fi",
		Isolation: "Sandbox", Network: "None", Timeout: 10 * time.Second,
		DeniedPaths: []string{deniedDirectory}, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for process.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if process.Alive() {
		t.Fatal("sandboxed shell did not complete")
	}
	if err := process.WaitError(); err != nil {
		t.Fatalf("sandbox did not enforce the denied path: %v; stderr=%s", err, process.Stderr())
	}
	if !strings.Contains(output.String(), "denied\n") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestFastShellRecordsSpawnBeforeItCanBeReaped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native shell execution requires macOS")
	}
	spawnedPID := 0
	process, err := StartShell(context.Background(), ShellConfig{
		Layout: NewLayout(t.TempDir()), Script: "true", Isolation: "Host", Network: "Outbound",
		Timeout: 10 * time.Second,
		OnSpawn: func(pid int) error {
			spawnedPID = pid
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spawnedPID <= 0 || process.PID() != spawnedPID {
		t.Fatalf("spawned PID = %d, process PID = %d", spawnedPID, process.PID())
	}
	deadline := time.Now().Add(10 * time.Second)
	for process.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if process.Alive() {
		t.Fatal("fast shell did not complete")
	}
	if err := process.WaitError(); err != nil {
		t.Fatal(err)
	}
}
