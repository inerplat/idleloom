package serviceinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLaunchdPlistEscapesArgumentsAndIsValid(t *testing.T) {
	data := launchdPlist(service{
		label: "io.idleloom.agent.test", program: "/tmp/idleloom & native",
		arguments:   []string{"--state-dir", "/tmp/state & trust"},
		environment: []string{"PATH=/usr/bin:/bin"},
		stdout:      "/tmp/native.log", stderr: "/tmp/native.log",
	})
	if bytes.Contains(data, []byte("idleloom & native")) || !bytes.Contains(data, []byte("idleloom &amp; native")) {
		t.Fatalf("plist did not XML-escape values: %s", data)
	}
	if !bytes.Contains(data, []byte("/usr/bin/env")) || !bytes.Contains(data, []byte("<string>-i</string>")) {
		t.Fatalf("plist does not clear inherited launchd environment: %s", data)
	}
	if !bytes.Contains(data, []byte("<key>ProcessType</key><string>Background</string>")) {
		t.Fatalf("default service process type is not Background: %s", data)
	}
	command := exec.Command("plutil", "-lint", "-")
	command.Stdin = bytes.NewReader(data)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected generated plist: %v: %s", err, output)
	}
}

func TestLaunchdPlistAllowsStandardComputeAgent(t *testing.T) {
	data := launchdPlist(service{label: "io.idleloom.agent.test", program: "/tmp/idlectl", processType: "Standard"})
	if !bytes.Contains(data, []byte("<key>ProcessType</key><string>Standard</string>")) {
		t.Fatalf("compute agent process type is not Standard: %s", data)
	}
}

func TestLabelSuffixIsStableAndSafe(t *testing.T) {
	if got := labelSuffix("Studio Mac.local"); got != "studio-mac-local" {
		t.Fatalf("labelSuffix = %q", got)
	}
}

func TestWriteReceiptIsPrivateAndComplete(t *testing.T) {
	directory := t.TempDir()
	want := Receipt{Version: 1, HostID: "test", UserLabels: []string{"io.idleloom.agent.test"}}
	if err := writeReceipt(directory, want); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, receiptFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Receipt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || len(got.UserLabels) != 1 || got.UserLabels[0] != want.UserLabels[0] {
		t.Fatalf("receipt = %#v, want %#v", got, want)
	}
}

func TestRootArtifactsAreDerivedFromValidatedLabel(t *testing.T) {
	label := "io.idleloom.link.studio-one"
	helper, plist, state, err := rootArtifacts(label)
	if err != nil {
		t.Fatal(err)
	}
	if helper != "/Library/PrivilegedHelperTools/"+label || plist != "/Library/LaunchDaemons/"+label+".plist" {
		t.Fatalf("unexpected root artifacts: helper=%q plist=%q", helper, plist)
	}
	if state != "/Library/Application Support/Idleloom/Native/"+label {
		t.Fatalf("root state = %q", state)
	}
	for _, malicious := range []string{
		"io.idleloom.link.",
		"io.idleloom.link../../tmp/owned",
		"io.idleloom.link.test/../../tmp/owned",
		"other.link.test",
	} {
		if _, _, _, err := rootArtifacts(malicious); err == nil {
			t.Fatalf("accepted unsafe root label %q", malicious)
		}
	}
}

func TestReceiptRejectsUntrustedCleanupPaths(t *testing.T) {
	valid := Receipt{
		Version:    1,
		HostID:     "studio-one",
		UserLabels: []string{"io.idleloom.agent.studio-one"},
		RootLabel:  "io.idleloom.link.studio-one",
		RootPhase:  "owned",
	}
	if err := validateReceipt(valid); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []Receipt{
		{Version: 2, HostID: "test"},
		{Version: 1},
		{Version: 1, HostID: "test", UserLabels: []string{"../../Library/LaunchAgents/other"}},
		{Version: 1, HostID: "test", UserLabels: []string{"io.idleloom.agent.other"}},
		{Version: 1, HostID: "test", RootLabel: "io.idleloom.link.other", RootPhase: "owned"},
		{Version: 1, HostID: "test", RootLabel: "io.idleloom.link.test"},
		{Version: 1, HostID: "test", RootPhase: "planned"},
	} {
		if err := validateReceipt(receipt); err == nil {
			t.Fatalf("accepted unsafe receipt %#v", receipt)
		}
	}
}

func TestCanonicalPathResolvesStateDirectoryAliases(t *testing.T) {
	directory := t.TempDir()
	alias := directory + "-alias"
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(alias) }()
	got, err := canonicalPath(alias)
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonicalPath(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("canonical alias = %q, want %q", got, want)
	}
}

func TestRootOwnershipBindsLabelAndStateDirectory(t *testing.T) {
	receipt := Receipt{Version: 1, HostID: "studio", RootLabel: "io.idleloom.link.studio", RootPhase: "owned"}
	owner := rootReceipt{
		Version: 1, HostID: "studio", Label: receipt.RootLabel, StateDirectory: "/state/studio",
	}
	if err := validateRootOwnership(owner, receipt, "/state/studio"); err != nil {
		t.Fatal(err)
	}
	owner.StateDirectory = "/state/other"
	if err := validateRootOwnership(owner, receipt, "/state/studio"); err == nil {
		t.Fatal("accepted privileged ownership from another state directory")
	}
	owner.StateDirectory = "/state/studio"
	owner.Label = "io.idleloom.link.other"
	if err := validateRootOwnership(owner, receipt, "/state/studio"); err == nil {
		t.Fatal("accepted privileged ownership from another service label")
	}
}

func TestBinaryIdentityComparison(t *testing.T) {
	if !sameBinary([]byte("same"), []byte("same")) {
		t.Fatal("identical binaries did not match")
	}
	if sameBinary([]byte("public"), []byte("replaced")) {
		t.Fatal("replaced link companion matched public binary")
	}
}

func TestWriteBinaryInstallsOnlyProvidedCanonicalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin", "idlectl")
	want := []byte("canonical-idlectl")
	if err := writeBinary(path, want, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("installed bytes = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("installed mode = %o, want 700", info.Mode().Perm())
	}
}

func TestResolveSudoArgumentsUsesAbsoluteAllowlist(t *testing.T) {
	arguments, err := resolveSudoArguments([]string{"launchctl", "bootout", "system/io.idleloom.link.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(arguments) != 3 || arguments[0] != "/bin/launchctl" {
		t.Fatalf("resolved arguments = %#v", arguments)
	}
	if _, err := resolveSudoArguments([]string{"sh", "-c", "true"}); err == nil {
		t.Fatal("unsupported privileged command was accepted")
	}
}

func TestLaunchdNotLoadedErrorsAreExpected(t *testing.T) {
	for _, message := range []string{"Could not find specified service", "Boot-out failed: 3: No such process"} {
		if !isNotLoaded(errors.New(message)) {
			t.Fatalf("not-loaded error %q was not recognized", message)
		}
	}
}

func TestCapturedBinaryMatchesRunningCodeIdentity(t *testing.T) {
	data, err := CaptureCurrentBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("captured binary is empty")
	}
}

func TestRootRemovalPreflightAuthenticatesBeforeMutation(t *testing.T) {
	receipt := Receipt{
		Version: 1, HostID: "studio", RootLabel: "io.idleloom.link.studio", RootPhase: "owned",
	}
	called := false
	wantErr := errors.New("authorization denied")
	err := preflightRootRemoval(context.Background(), "/tmp/state", receipt, func(_ context.Context, stateDirectory string, got Receipt, rootStateDirectory string) error {
		called = true
		if stateDirectory != "/tmp/state" || got.HostID != receipt.HostID || got.RootLabel != receipt.RootLabel || got.RootPhase != receipt.RootPhase || rootStateDirectory != "/Library/Application Support/Idleloom/Native/io.idleloom.link.studio" {
			t.Fatalf("preflight arguments: state=%q receipt=%#v root=%q", stateDirectory, got, rootStateDirectory)
		}
		return wantErr
	})
	if !called || !errors.Is(err, wantErr) {
		t.Fatalf("preflight called=%t err=%v", called, err)
	}
}

func TestPlannedRootRemovalSkipsOwnershipPreflight(t *testing.T) {
	receipt := Receipt{
		Version: 1, HostID: "studio", RootLabel: "io.idleloom.link.studio", RootPhase: "planned",
	}
	if err := preflightRootRemoval(context.Background(), "/tmp/state", receipt, func(context.Context, string, Receipt, string) error {
		t.Fatal("planned cleanup attempted to verify an ownership record that may not exist")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipTemporaryNameIsStrict(t *testing.T) {
	if !validOwnershipTemporaryName("service-owner.json.idleloom-123.new") {
		t.Fatal("valid ownership temporary name was rejected")
	}
	for _, name := range []string{
		"service-owner.json",
		"service-owner.json.idleloom-.new",
		"service-owner.json.idleloom-12x.new",
		"../service-owner.json.idleloom-123.new",
		"wirekube-leaf.json",
	} {
		if validOwnershipTemporaryName(name) {
			t.Fatalf("unsafe ownership temporary name %q was accepted", name)
		}
	}
}
