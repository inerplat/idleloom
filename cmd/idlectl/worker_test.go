package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inerplat/idleloom/internal/idleloom"
	"github.com/spf13/pflag"
)

func TestPromptCancelsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked, _ := io.Pipe()
	if _, err := prompt(ctx, bufio.NewReader(blocked), "Node name", "mac"); err == nil {
		t.Fatal("a cancelled context must abort the prompt")
	}
}

func TestPromptCancelsOnEndOfInputWithoutAnswer(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(""))
	if _, err := prompt(context.Background(), reader, "Create this worker?", "yes"); err == nil {
		t.Fatal("EOF without an answer must cancel instead of accepting the default")
	}
}

func TestPromptKeepsDefaultAndTypedAnswers(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\ncustom\n"))
	value, err := prompt(context.Background(), reader, "Memory", "8g")
	if err != nil || value != "8g" {
		t.Fatalf("empty line must keep the default, got %q err %v", value, err)
	}
	value, err = prompt(context.Background(), reader, "Disk", "40g")
	if err != nil || value != "custom" {
		t.Fatalf("typed answer must win, got %q err %v", value, err)
	}
}

func TestExplicitFlagsTracksOnlyCommandLineValues(t *testing.T) {
	flags := pflag.NewFlagSet("create worker", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Int("cpus", 4, "")
	flags.String("memory", "8g", "")
	flags.String("disk", "40g", "")
	if err := flags.Parse([]string{"--disk", "60g", "--cpus", "4"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	explicit := explicitFlags(flags)
	if !explicit["disk"] || !explicit["cpus"] {
		t.Fatalf("flags passed on the command line must be explicit, got %v", explicit)
	}
	if explicit["memory"] {
		t.Fatalf("memory was not passed and must not be explicit, got %v", explicit)
	}
}

func TestParseSizeMiB(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{input: "8g", want: 8192},
		{input: "8gb", want: 8192},
		{input: "8gib", want: 8192},
		{input: "8192m", want: 8192},
		{input: "8192mb", want: 8192},
		{input: "8192mib", want: 8192},
		{input: "512", want: 512},
		{input: " 8G ", want: 8192},
		{input: "8 g", want: 8192},
		{input: "", wantErr: true},
		{input: "eight", wantErr: true},
		{input: "8t", wantErr: true},
		{input: "0g", wantErr: true},
		{input: "-1g", wantErr: true},
		{input: "-8192m", wantErr: true},
	}
	for _, test := range tests {
		got, err := parseSizeMiB(test.input)
		if test.wantErr {
			if err == nil {
				t.Fatalf("parseSizeMiB(%q) = %d, expected an error", test.input, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseSizeMiB(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("parseSizeMiB(%q) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestDefaultNamesProducesSanitizedNodeName(t *testing.T) {
	name := defaultNames()
	if !strings.HasSuffix(name, "-idle") {
		t.Fatalf("default node name %q does not end with -idle", name)
	}
	if strings.HasPrefix(name, "-") {
		t.Fatalf("default node name %q starts with a dash", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		t.Fatalf("default node name %q contains unsupported rune %q", name, r)
	}
}

func savedWorkerState(t *testing.T, nodeName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := idleloom.SaveState(path, idleloom.State{
		NodeName: nodeName,
		Phase:    idleloom.PhaseReady,
		Runtime:  idleloom.RuntimeState{NodeName: nodeName},
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerVerbsAreDispatchedPublicly(t *testing.T) {
	for _, command := range []string{"create", "start", "stop", "status", "get", "delete"} {
		handled, err := runPublicCommand(context.Background(), command, []string{"--help"})
		if !handled || !errors.Is(err, pflag.ErrHelp) {
			t.Fatalf("%s --help handled=%t err=%v", command, handled, err)
		}
	}
}

func TestLegacyWorkerNamespaceReturnsMigrationHint(t *testing.T) {
	for _, args := range [][]string{nil, {"init"}, {"init", "--name", "mac-idle"}, {"maintain"}, {"status"}} {
		handled, err := runPublicCommand(context.Background(), "worker", args)
		if !handled {
			t.Fatalf("worker %v was not handled", args)
		}
		if err == nil || !strings.Contains(err.Error(), "worker subcommands moved") {
			t.Fatalf("worker %v error = %v", args, err)
		}
		if !isUsageError(err) {
			t.Fatalf("worker %v migration hint must exit with usage status 2", args)
		}
	}
}

func TestHelpRoutesToCommandHelp(t *testing.T) {
	handled, err := runPublicCommand(context.Background(), "help", []string{"join"})
	if !handled || !errors.Is(err, pflag.ErrHelp) {
		t.Fatalf("help join handled=%t err=%v", handled, err)
	}
	handled, err = runPublicCommand(context.Background(), "help", nil)
	if !handled || err != nil {
		t.Fatalf("bare help handled=%t err=%v", handled, err)
	}
	handled, err = runPublicCommand(context.Background(), "help", []string{"help"})
	if !handled || err != nil {
		t.Fatalf("help help handled=%t err=%v", handled, err)
	}
	handled, err = runPublicCommand(context.Background(), "help", []string{"no-such-command"})
	if !handled || err == nil || !isUsageError(err) {
		t.Fatalf("help for an unknown topic handled=%t err=%v", handled, err)
	}
}

func TestCreateWorkerRequiresNameWithYesOrDryRun(t *testing.T) {
	for _, args := range [][]string{{"worker", "--yes"}, {"worker", "--dry-run"}} {
		err := runCreateWorker(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "NAME is required") {
			t.Fatalf("create %v error = %v", args, err)
		}
		if !isUsageError(err) {
			t.Fatalf("create %v must be a usage error", args)
		}
	}
}

func TestCreateWorkerRejectsLegacyNameFlagAndExtraPositionals(t *testing.T) {
	if err := runCreateWorker(context.Background(), []string{"worker", "--name", "mac-idle", "--yes"}); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("legacy --name flag error = %v", err)
	}
	if err := runCreateWorker(context.Background(), []string{"worker", "a", "b", "--yes"}); err == nil || !isUsageError(err) {
		t.Fatalf("extra positionals error = %v", err)
	}
	if err := runCreateWorker(context.Background(), []string{"host", "a", "--yes"}); err == nil || !isUsageError(err) {
		t.Fatalf("non-worker resource error = %v", err)
	}
}

func TestCreateWorkerValidatesNameBeforeAnyHostOrClusterChange(t *testing.T) {
	err := runCreateWorker(context.Background(), []string{"worker", "Bad_Name", "--yes"})
	if err == nil || !strings.Contains(err.Error(), "invalid node name") {
		t.Fatalf("invalid node name error = %v", err)
	}
}

func TestStartAndStopWorkerValidateTheRequestedName(t *testing.T) {
	statePath := savedWorkerState(t, "worker-a")
	err := runStartWorker(context.Background(), []string{"worker", "ghost", "--state", statePath})
	if err == nil || !strings.Contains(err.Error(), `this Mac's worker is "worker-a", not "ghost"`) {
		t.Fatalf("start mismatch error = %v", err)
	}
	err = runStopWorker(context.Background(), []string{"worker", "ghost", "--state", statePath})
	if err == nil || !strings.Contains(err.Error(), `this Mac's worker is "worker-a", not "ghost"`) {
		t.Fatalf("stop mismatch error = %v", err)
	}
}

func TestDeleteWorkerRequiresMatchingNameAsConfirmation(t *testing.T) {
	if err := runDelete(context.Background(), []string{"worker"}); err == nil || !strings.Contains(err.Error(), "requires its NAME") {
		t.Fatalf("delete worker without NAME error = %v", err)
	}
	statePath := savedWorkerState(t, "worker-a")
	err := runDelete(context.Background(), []string{"--state", statePath, "worker", "ghost"})
	if err == nil || !strings.Contains(err.Error(), `this Mac's worker is "worker-a", not "ghost"`) {
		t.Fatalf("delete worker mismatch error = %v", err)
	}
}

func TestDeleteWorkerWithoutStateIsFriendly(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	err := runDelete(context.Background(), []string{"--state", statePath, "worker", "ghost"})
	if err == nil || !strings.Contains(err.Error(), "no Idleloom worker exists on this Mac") {
		t.Fatalf("delete worker without state error = %v", err)
	}
	if strings.Contains(err.Error(), statePath) {
		t.Fatalf("friendly error leaks the raw state path: %v", err)
	}
}

func TestDeleteSeparatesWorkerAndClusterFlags(t *testing.T) {
	if err := runDelete(context.Background(), []string{"--force", "workload/job"}); err == nil || !strings.Contains(err.Error(), "only applies to workers") {
		t.Fatalf("workload --force error = %v", err)
	}
	if err := runDelete(context.Background(), []string{"--wait=false", "worker", "mac-idle"}); err == nil || !strings.Contains(err.Error(), "does not apply to workers") {
		t.Fatalf("worker --wait error = %v", err)
	}
}

func TestGetWorkersWithoutStatePrintsFriendlyEmptyResult(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	var output bytes.Buffer
	if err := getWorkers(context.Background(), &output, "", "", statePath, "", "table"); err != nil {
		t.Fatalf("getWorkers: %v", err)
	}
	if !strings.Contains(output.String(), "idlectl create worker") {
		t.Fatalf("empty result does not mention create worker: %q", output.String())
	}
}

func TestGetWorkersReportsNameMismatchWithoutClusterAccess(t *testing.T) {
	statePath := savedWorkerState(t, "worker-a")
	var output bytes.Buffer
	err := getWorkers(context.Background(), &output, "", "", statePath, "ghost", "table")
	if err == nil || !strings.Contains(err.Error(), `this Mac's worker is "worker-a"`) {
		t.Fatalf("getWorkers mismatch error = %v", err)
	}
}

func TestStatusReportsAbsenceWithoutError(t *testing.T) {
	stateDirectory := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	var output bytes.Buffer
	if err := runStatus(context.Background(), []string{"--state-dir", stateDirectory, "--state", statePath}, &output); err != nil {
		t.Fatalf("status on an empty machine: %v", err)
	}
	if !strings.Contains(output.String(), "not joined") || !strings.Contains(output.String(), "no worker") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestStatusReportsExistingWorkerNameAndPhase(t *testing.T) {
	stateDirectory := t.TempDir()
	statePath := savedWorkerState(t, "worker-a")
	var output bytes.Buffer
	if err := runStatus(context.Background(), []string{"--state-dir", stateDirectory, "--state", statePath}, &output); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(output.String(), "worker-a (ready)") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestInternalMaintainIsDispatched(t *testing.T) {
	handled, err := runInternalCommand(context.Background(), []string{"internal", "maintain", "--help"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("internal maintain --help handled=%t err=%v", handled, err)
	}
}
