package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/inerplat/idleloom/internal/idleloom"
	"github.com/inerplat/idleloom/internal/native/enroll"
	"github.com/spf13/pflag"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	createWorkerUsage = "idlectl create worker NAME [flags]"
	startWorkerUsage  = "idlectl start worker [NAME] [flags]"
	stopWorkerUsage   = "idlectl stop worker [NAME] [flags]"
	statusUsage       = "idlectl status [flags]"
	workerStateHelp   = "Idleloom worker state file (default ~/.idleloom/state.json)"
)

func workerPFlags(name, usage string) *pflag.FlagSet {
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage:\n  %s\n\nFlags:\n", usage)
		flags.PrintDefaults()
	}
	return flags
}

// workerNameArgument extracts the optional worker NAME from the positional
// arguments of a worker verb, requiring the "worker" resource token.
func workerNameArgument(positionals []string, usage string) (string, error) {
	if len(positionals) == 0 {
		return "", usagef("usage: %s", usage)
	}
	resourceName, name, err := parseResourceReference(positionals, false)
	if err != nil || resourceName != resourceWorkers {
		return "", usagef("usage: %s", usage)
	}
	return name, nil
}

func runCreateWorker(ctx context.Context, args []string) error {
	flags := workerPFlags("create worker", createWorkerUsage)
	kubeconfig := flags.String("kubeconfig", "", "kubeconfig used to enroll the worker")
	contextName := flags.String("context", "", "kubeconfig context (defaults to current-context)")
	cpus := flags.Int("cpus", 4, "worker CPU count")
	memory := flags.String("memory", defaultMemory(), "worker memory, for example 8g")
	disk := flags.String("disk", "40g", "worker disk size, for example 40g")
	taint := flags.String("taint", "idleloom-dedicated=compute:NoSchedule", "taint registered on the dedicated worker; empty disables")
	network := flags.String("network", idleloom.NetworkWireKube, "node network (currently wirekube)")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait per enrollment stage")
	tokenTTL := flags.Duration("token-ttl", 30*time.Minute, "bootstrap token lifetime")
	waitForReady := flags.Bool("wait", true, "wait for WireKube and Kubernetes Node readiness")
	statePath := flags.String("state", "", workerStateHelp)
	runtimeDir := flags.String("runtime-dir", "", "worker runtime directory (advanced)")
	registryMirrors := flags.StringArray("registry-mirror", nil, "redirect image pulls for a registry to a mirror, as HOST=URL (advanced, repeatable)")
	credentialProviderBins := flags.StringArray("credential-provider-bin", nil, "host path to a linux/arm64 kubelet image credential provider binary (advanced, repeatable)")
	credentialProviderConfig := flags.String("credential-provider-config", "", "host path to a kubelet CredentialProviderConfig YAML (advanced)")
	credentialProviderEnvFile := flags.String("credential-provider-env-file", "", "host path to a KEY=VALUE env file for the credential providers (advanced, optional)")
	yes := flags.Bool("yes", false, "accept defaults without prompting")
	dryRun := flags.Bool("dry-run", false, "run preflight checks without changing the host or cluster")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name, err := workerNameArgument(flags.Args(), createWorkerUsage)
	if err != nil {
		return err
	}
	if name == "" && (*yes || *dryRun) {
		return usagef("a worker NAME is required with --yes or --dry-run; usage: %s", createWorkerUsage)
	}

	if !*yes && !*dryRun {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("interactive input is unavailable; pass --yes to accept defaults")
		}
		explicit := explicitFlags(flags)
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("Idleloom turns this Mac into an after-hours Kubernetes worker.")
		if name == "" {
			if name, err = prompt(ctx, reader, "Node name", defaultNames()); err != nil {
				return err
			}
		}
		if !explicit["cpus"] {
			if *cpus, err = promptInt(ctx, reader, "CPU cores", *cpus); err != nil {
				return err
			}
		}
		if !explicit["memory"] {
			if *memory, err = prompt(ctx, reader, "Memory", *memory); err != nil {
				return err
			}
		}
		if !explicit["disk"] {
			if *disk, err = prompt(ctx, reader, "Disk", *disk); err != nil {
				return err
			}
		}
		if !explicit["network"] {
			if *network, err = prompt(ctx, reader, "Network", *network); err != nil {
				return err
			}
		}
		fmt.Printf("Worker: %s (%d CPUs, %s memory, %s disk, %s network)\n", name, *cpus, *memory, *disk, *network)
		answer, err := prompt(ctx, reader, "Create this worker?", "yes")
		if err != nil {
			return err
		}
		answer = strings.ToLower(answer)
		if answer != "yes" && answer != "y" {
			return fmt.Errorf("cancelled")
		}
	}

	memoryMB, err := parseSizeMiB(*memory)
	if err != nil {
		return fmt.Errorf("invalid --memory: %w", err)
	}
	diskMB, err := parseSizeMiB(*disk)
	if err != nil {
		return fmt.Errorf("invalid --disk: %w", err)
	}
	if len(*credentialProviderBins) > 0 || *credentialProviderConfig != "" || *credentialProviderEnvFile != "" {
		if *credentialProviderConfig == "" {
			return usagef("--credential-provider-config is required when configuring credential providers; usage: %s", createWorkerUsage)
		}
		if len(*credentialProviderBins) == 0 {
			return usagef("at least one --credential-provider-bin is required when configuring credential providers; usage: %s", createWorkerUsage)
		}
	}
	app := idleloom.NewApp(os.Stdout, os.Stderr)
	return app.Init(ctx, idleloom.InitOptions{
		KubeconfigPath: *kubeconfig,
		Context:        *contextName,
		NodeName:       name,
		CPUs:           *cpus,
		MemoryMB:       memoryMB,
		DiskMB:         diskMB,
		Taint:          *taint,
		Network:        *network,
		Timeout:        *timeout,
		TokenTTL:       *tokenTTL,
		SkipWait:       !*waitForReady,
		StatePath:      *statePath,
		RuntimeDir:     *runtimeDir,
		DryRun:         *dryRun,

		RegistryMirrors:          *registryMirrors,
		CredentialProviderBins:   *credentialProviderBins,
		CredentialProviderConfig: *credentialProviderConfig,
		CredentialProviderEnv:    *credentialProviderEnvFile,
	})
}

func runStartWorker(ctx context.Context, args []string) error {
	flags := workerPFlags("start worker", startWorkerUsage)
	statePath := flags.String("state", "", workerStateHelp)
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait for worker recovery")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name, err := workerNameArgument(flags.Args(), startWorkerUsage)
	if err != nil {
		return err
	}
	if name != "" {
		if err := ensureLocalWorkerNamed(*statePath, name); err != nil {
			return err
		}
	}
	return idleloom.NewApp(os.Stdout, os.Stderr).Start(ctx, *statePath, *timeout)
}

func runStopWorker(ctx context.Context, args []string) error {
	flags := workerPFlags("stop worker", stopWorkerUsage)
	statePath := flags.String("state", "", workerStateHelp)
	localOnly := flags.Bool("local-only", false, "stop the local VM without contacting Kubernetes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	name, err := workerNameArgument(flags.Args(), stopWorkerUsage)
	if err != nil {
		return err
	}
	if name != "" {
		if err := ensureLocalWorkerNamed(*statePath, name); err != nil {
			return err
		}
	}
	return idleloom.NewApp(os.Stdout, os.Stderr).Stop(ctx, *statePath, *localOnly)
}

// deleteWorker removes this Mac's worker. NAME is required as a confirmation
// affordance and must match the locally recorded worker.
func deleteWorker(ctx context.Context, statePath, name string, force, localOnly bool) error {
	if name == "" {
		return usagef(`deleting a worker requires its NAME as confirmation; run "idlectl get workers" to see this Mac's worker`)
	}
	if err := ensureLocalWorkerNamed(statePath, name); err != nil {
		return err
	}
	return idleloom.NewApp(os.Stdout, os.Stderr).Delete(ctx, statePath, force, localOnly)
}

// runStatus prints a local overview of this Mac: the Native Metal enrollment
// and the Idleloom worker. Absence of either is a normal answer, not an error.
func runStatus(_ context.Context, args []string, out io.Writer) error {
	flags := workerPFlags("status", statusUsage)
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private Native Metal enrollment state")
	statePath := flags.String("state", "", workerStateHelp)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return usagef("usage: %s", statusUsage)
	}
	identity, err := enroll.IdentityFromState(*stateDirectory)
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "Native Metal host: %s (joined)\n", identity.HostID)
	case errors.Is(err, fs.ErrNotExist):
		_, _ = fmt.Fprintln(out, "Native Metal host: not joined")
	default:
		return fmt.Errorf("read enrolled host identity: %w", err)
	}
	path, err := workerStatePath(*statePath)
	if err != nil {
		return err
	}
	exists, err := workerStateExists(path)
	if err != nil {
		return err
	}
	if !exists {
		_, _ = fmt.Fprintln(out, "Worker:            no worker")
		return nil
	}
	state, err := idleloom.LoadState(path)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Worker:            %s (%s)\n", state.NodeName, state.Phase)
	return nil
}

type workerRow struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	NodeReady string `json:"nodeReady"`
	Age       string `json:"age"`
}

func getWorkers(ctx context.Context, out io.Writer, kubeconfig, kubeContext, statePath, name, output string) error {
	path, err := workerStatePath(statePath)
	if err != nil {
		return err
	}
	exists, err := workerStateExists(path)
	if err != nil {
		return err
	}
	if !exists {
		if name != "" {
			return fmt.Errorf(`worker %q was not found: no Idleloom worker exists on this Mac; workers are created with "idlectl create worker NAME"`, name)
		}
		_, _ = fmt.Fprintln(out, `No Idleloom worker exists on this Mac. Create one with "idlectl create worker NAME".`)
		return nil
	}
	state, err := idleloom.LoadState(path)
	if err != nil {
		return err
	}
	if name != "" && name != state.NodeName {
		return fmt.Errorf("worker %q was not found; this Mac's worker is %q", name, state.NodeName)
	}
	if kubeconfig == "" {
		kubeconfig = state.KubeconfigPath
	}
	if kubeContext == "" {
		kubeContext = state.Context
	}
	config, err := loadConfig(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	nodeReady := "Absent"
	node, err := clientset.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{})
	switch {
	case err == nil:
		nodeReady = workerNodeReadyStatus(node)
	case apierrors.IsNotFound(err):
	default:
		return fmt.Errorf("get worker node %s: %w", state.NodeName, err)
	}
	age := "-"
	if !state.CreatedAt.IsZero() {
		age = time.Since(state.CreatedAt).Round(time.Second).String()
	}
	if output != "table" {
		return printStructured(workerRow{Name: state.NodeName, Phase: state.Phase, NodeReady: nodeReady, Age: age}, output)
	}
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tPHASE\tNODE READY\tAGE")
	_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", state.NodeName, state.Phase, nodeReady, age)
	return writer.Flush()
}

func workerNodeReadyStatus(node *corev1.Node) string {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return string(condition.Status)
		}
	}
	return "Unknown"
}

func workerStatePath(statePath string) (string, error) {
	if statePath != "" {
		return statePath, nil
	}
	return idleloom.DefaultStatePath()
}

// workerStateExists reports whether a worker state file is present. It checks
// the path directly so the answer does not depend on how LoadState words its
// missing-file error.
func workerStateExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect worker state: %w", err)
	}
	return true, nil
}

func loadLocalWorkerState(statePath string) (idleloom.State, error) {
	path, err := workerStatePath(statePath)
	if err != nil {
		return idleloom.State{}, err
	}
	exists, err := workerStateExists(path)
	if err != nil {
		return idleloom.State{}, err
	}
	if !exists {
		return idleloom.State{}, fmt.Errorf(`no Idleloom worker exists on this Mac; create one with "idlectl create worker NAME"`)
	}
	return idleloom.LoadState(path)
}

func ensureLocalWorkerNamed(statePath, name string) error {
	state, err := loadLocalWorkerState(statePath)
	if err != nil {
		return err
	}
	if state.NodeName != name {
		return fmt.Errorf("this Mac's worker is %q, not %q", state.NodeName, name)
	}
	return nil
}

// localWorkerNodeName reports the node name of this Mac's worker from the
// default state file, or "" when no worker state exists or it is unreadable.
func localWorkerNodeName() string {
	path, err := idleloom.DefaultStatePath()
	if err != nil {
		return ""
	}
	state, err := idleloom.LoadState(path)
	if err != nil {
		return ""
	}
	return state.NodeName
}

// explicitFlags reports which flags were set on the command line, so the
// interactive wizard only prompts for values the user did not provide.
func explicitFlags(flags *pflag.FlagSet) map[string]bool {
	set := map[string]bool{}
	flags.Visit(func(f *pflag.Flag) { set[f.Name] = true })
	return set
}

func defaultNames() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "mac"
	}
	hostname = strings.ToLower(strings.Split(hostname, ".")[0])
	var clean strings.Builder
	for _, r := range hostname {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			clean.WriteRune(r)
		} else {
			clean.WriteByte('-')
		}
	}
	name := strings.Trim(clean.String(), "-")
	if name == "" {
		name = "mac"
	}
	return name + "-idle"
}

func defaultMemory() string {
	return "8g"
}

func parseSizeMiB(value string) (int, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	multiplier := 1
	switch {
	case strings.HasSuffix(value, "gib"):
		multiplier = 1024
		value = strings.TrimSuffix(value, "gib")
	case strings.HasSuffix(value, "gb"):
		multiplier = 1024
		value = strings.TrimSuffix(value, "gb")
	case strings.HasSuffix(value, "g"):
		multiplier = 1024
		value = strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "mib"):
		value = strings.TrimSuffix(value, "mib")
	case strings.HasSuffix(value, "mb"):
		value = strings.TrimSuffix(value, "mb")
	case strings.HasSuffix(value, "m"):
		value = strings.TrimSuffix(value, "m")
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("expected a positive size such as 8g or 8192m")
	}
	return number * multiplier, nil
}

type promptAnswer struct {
	line string
	err  error
}

// prompt reads one wizard answer. Ctrl-C (context cancellation) aborts the
// blocked read, and end of input (Ctrl-D) without an answer cancels the
// wizard instead of silently accepting the default.
func prompt(ctx context.Context, reader *bufio.Reader, label, defaultValue string) (string, error) {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	answers := make(chan promptAnswer, 1)
	go func() {
		line, err := reader.ReadString('\n')
		answers <- promptAnswer{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		fmt.Println()
		return "", ctx.Err()
	case answer := <-answers:
		line := strings.TrimSpace(answer.line)
		if line != "" {
			return line, nil
		}
		if answer.err != nil {
			fmt.Println()
			return "", fmt.Errorf("cancelled")
		}
		return defaultValue, nil
	}
}

func promptInt(ctx context.Context, reader *bufio.Reader, label string, defaultValue int) (int, error) {
	value, err := prompt(ctx, reader, label, strconv.Itoa(defaultValue))
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue, nil
	}
	return parsed, nil
}
