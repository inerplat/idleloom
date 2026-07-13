package devruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEmbeddedLocks(t *testing.T) {
	runtimeFiles, runtimeDigest, err := RuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtimeFiles) != 34 || runtimeDigest != "45ddb5b2b3313ddd27a9edb58ece974b70452275e3e92a1ba3672e9cff5ace3d" {
		t.Fatalf("unexpected runtime lock: files=%d digest=%s", len(runtimeFiles), runtimeDigest)
	}
	modelFiles, modelDigest, err := ModelLock()
	if err != nil {
		t.Fatal(err)
	}
	if len(modelFiles) != 9 || modelDigest != "dbac28dadd17eb15750fcc92aa9f05181ab1b7f574dbae66bd73a73977eec44f" {
		t.Fatalf("unexpected model lock: files=%d digest=%s", len(modelFiles), modelDigest)
	}
}

func TestLockedModelDescriptorMatchesEmbeddedLock(t *testing.T) {
	descriptor, err := LockedModel()
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "qwen3-5-0-8b-mlx" || descriptor.SizeBytes != 652019388 || descriptor.ManifestDigest != "sha256:dbac28dadd17eb15750fcc92aa9f05181ab1b7f574dbae66bd73a73977eec44f" {
		t.Fatalf("locked model descriptor = %#v", descriptor)
	}
	if descriptor.ArtifactIdentity != "oci://development.invalid/idleloom/qwen3.5-0.8b-4bit@"+descriptor.ManifestDigest {
		t.Fatalf("artifact identity = %q", descriptor.ArtifactIdentity)
	}
}

func TestProcessStopWaitsForEntireProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-specific")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := &Process{cmd: cmd, done: make(chan struct{}), stderr: &boundedBuffer{limit: 1024}}
	go func() {
		process.waitMu.Lock()
		process.waitErr = cmd.Wait()
		process.waitMu.Unlock()
		close(process.done)
	}()
	if err := process.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.done:
	case <-time.After(time.Second):
		t.Fatal("process was not reaped")
	}
}

func TestSandboxProfileProtectsCredentials(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox profile")
	}
	root := t.TempDir()
	layout := NewLayout(root)
	if err := os.MkdirAll(layout.Work, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(root, "..", "agent.kubeconfig")
	profile, err := sandboxProfile(layout, []string{credential})
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`(deny network*)`,
		`(deny file-read* (subpath "` + home + `"))`,
		`(deny file-read* (subpath "` + credential + `"))`,
		`com.apple.metalfe`,
		`file-issue-extension`,
	} {
		if !strings.Contains(profile, expected) {
			t.Fatalf("sandbox profile does not contain %q", expected)
		}
	}
}
