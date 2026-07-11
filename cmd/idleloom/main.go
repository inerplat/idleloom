package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inerplat/idleloom/internal/idleloom"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "idleloom: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	app := idleloom.NewApp(os.Stdout, os.Stderr)
	switch args[0] {
	case "init":
		return runInit(ctx, app, args[1:])
	case "status":
		flags := flag.NewFlagSet("status", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		statePath := flags.String("state", "", "Idleloom state file")
		if err := flags.Parse(args[1:]); err != nil {
			return flagParseError(err)
		}
		return app.Status(ctx, *statePath)
	case "start":
		flags := flag.NewFlagSet("start", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		statePath := flags.String("state", "", "Idleloom state file")
		timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait for worker recovery")
		if err := flags.Parse(args[1:]); err != nil {
			return flagParseError(err)
		}
		return app.Start(ctx, *statePath, *timeout)
	case "stop":
		flags := flag.NewFlagSet("stop", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		statePath := flags.String("state", "", "Idleloom state file")
		localOnly := flags.Bool("local-only", false, "stop the local VM without contacting Kubernetes")
		if err := flags.Parse(args[1:]); err != nil {
			return flagParseError(err)
		}
		return app.Stop(ctx, *statePath, *localOnly)
	case "delete":
		flags := flag.NewFlagSet("delete", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		statePath := flags.String("state", "", "Idleloom state file")
		force := flags.Bool("force", false, "delete even when workload Pods are active")
		localOnly := flags.Bool("local-only", false, "delete local VM state without contacting Kubernetes")
		if err := flags.Parse(args[1:]); err != nil {
			return flagParseError(err)
		}
		return app.Delete(ctx, *statePath, *force, *localOnly)
	case "maintain":
		flags := flag.NewFlagSet("maintain", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		statePath := flags.String("state", "", "Idleloom state file")
		if err := flags.Parse(args[1:]); err != nil {
			return flagParseError(err)
		}
		return app.Maintain(ctx, *statePath)
	case "version", "--version", "-v":
		fmt.Printf("idleloom %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runInit(ctx context.Context, app *idleloom.App, args []string) error {
	defaults := defaultNames()
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	kubeconfig := flags.String("kubeconfig", "", "kubeconfig used to enroll the worker")
	contextName := flags.String("context", "", "kubeconfig context (defaults to current-context)")
	nodeName := flags.String("name", defaults, "Kubernetes node name")
	cpus := flags.Int("cpus", 4, "worker CPU count")
	memory := flags.String("memory", defaultMemory(), "worker memory, for example 8g")
	disk := flags.String("disk", "40g", "worker disk size, for example 40g")
	taint := flags.String("taint", "idleloom-dedicated=compute:NoSchedule", "taint registered on the dedicated worker; empty disables")
	network := flags.String("network", idleloom.NetworkWireKube, "node network (currently wirekube)")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait per enrollment stage")
	tokenTTL := flags.Duration("token-ttl", 30*time.Minute, "bootstrap token lifetime")
	statePath := flags.String("state", "", "Idleloom state file")
	runtimeDir := flags.String("runtime-dir", "", "worker runtime directory (advanced)")
	yes := flags.Bool("yes", false, "accept defaults without prompting")
	dryRun := flags.Bool("dry-run", false, "run preflight checks without changing the host or cluster")
	if err := flags.Parse(args); err != nil {
		return flagParseError(err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("init does not accept positional arguments")
	}

	if !*yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("interactive input is unavailable; pass --yes to accept defaults")
		}
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("Idleloom turns this Mac into an after-hours Kubernetes worker.")
		*nodeName = prompt(reader, "Node name", *nodeName)
		*cpus = promptInt(reader, "CPU cores", *cpus)
		*memory = prompt(reader, "Memory", *memory)
		*disk = prompt(reader, "Disk", *disk)
		*network = prompt(reader, "Network", *network)
		answer := strings.ToLower(prompt(reader, "Create this worker?", "yes"))
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
	return app.Init(ctx, idleloom.InitOptions{
		KubeconfigPath: *kubeconfig,
		Context:        *contextName,
		NodeName:       *nodeName,
		CPUs:           *cpus,
		MemoryMB:       memoryMB,
		DiskMB:         diskMB,
		Taint:          *taint,
		Network:        *network,
		Timeout:        *timeout,
		TokenTTL:       *tokenTTL,
		StatePath:      *statePath,
		RuntimeDir:     *runtimeDir,
		DryRun:         *dryRun,
	})
}

func flagParseError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
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

func prompt(reader *bufio.Reader, label, defaultValue string) string {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue
	}
	return line
}

func promptInt(reader *bufio.Reader, label string, defaultValue int) int {
	value := prompt(reader, label, strconv.Itoa(defaultValue))
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func printUsage() {
	fmt.Print(`Idleloom - weave idle Macs into Kubernetes compute

Usage:
  idleloom init [flags]
  idleloom status [flags]
  idleloom start [flags]
  idleloom stop [flags]
  idleloom delete [flags]
  idleloom version

Start with:
  idleloom init --kubeconfig ~/.kube/config
`)
}
