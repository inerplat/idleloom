package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/idleloom"
	nativeagent "github.com/inerplat/idleloom/internal/native/agent"
	nativecontroller "github.com/inerplat/idleloom/internal/native/controller"
	"github.com/inerplat/idleloom/internal/native/credential"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"github.com/inerplat/idleloom/internal/native/enroll"
	"github.com/inerplat/idleloom/internal/native/install"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/kubeletbridge"
	nativeprojection "github.com/inerplat/idleloom/internal/native/projection"
	"github.com/inerplat/idleloom/internal/native/serviceinstall"
	nativewirekube "github.com/inerplat/idleloom/internal/native/wirekube"
	"github.com/inerplat/idleloom/internal/native/wirekubecli"
	"github.com/spf13/pflag"
	"golang.org/x/term"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const usageText = `idlectl manages Idleloom compute on this Mac.

Native Metal — run macOS workloads on this Mac and project observability-only Nodes:
  idlectl join HOST [flags]
  idlectl run NAME --shell '<script>' [flags]
  idlectl recipe (list | show NAME@VERSION | render NAME@VERSION --name RUN) [flags]
  idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]

Worker — run a schedulable Kubernetes Node in a Linux VM on this Mac:
  idlectl create worker NAME [flags]
  idlectl start worker [NAME] [flags]
  idlectl stop worker [NAME] [flags]
  idlectl load (image REF...) [flags]

Shared:
  idlectl get (hosts|workers|workloads) [NAME] [flags]
  idlectl delete ((host|worker|workload) NAME | (host|worker|workload)/NAME) [flags]
  idlectl status
  idlectl version

Use "idlectl help COMMAND" for command flags.
`

// usageError marks command-line misuse (unknown or incomplete commands) so
// main can exit with status 2 instead of the generic failure status 1.
type usageError struct {
	message string
}

func (e usageError) Error() string {
	return e.message
}

func usagef(format string, args ...any) error {
	return usageError{message: fmt.Sprintf(format, args...)}
}

func isUsageError(err error) bool {
	var usage usageError
	return errors.As(err, &usage)
}

var (
	version   = "development"
	commit    = "unknown"
	buildDate = "unknown"
)

type wireKubeLifecycle interface {
	Plan(context.Context) (wirekubecli.Plan, error)
	Install(context.Context, wirekubecli.Plan) (wirekubecli.Result, error)
}

var newWireKubeLifecycle = func(ctx context.Context, kubeconfig, kubeContext string) (wireKubeLifecycle, error) {
	binary, err := (wirekubecli.Resolver{
		Version:        wirekubecli.CompatibleVersion,
		BinaryOverride: os.Getenv("IDLELOOM_WIREKUBECTL"),
	}).Resolve(ctx)
	if err != nil {
		return nil, err
	}
	return wirekubecli.Client{
		Binary: binary, ExpectedVersion: wirekubecli.CompatibleVersion,
		Kubeconfig: kubeconfig, Context: kubeContext, Timeout: 10 * time.Minute,
	}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	handled, internalErr := runInternalBinary(ctx, filepath.Base(os.Args[0]), os.Args[1:])
	if !handled && internalErr == nil {
		handled, internalErr = runInternalCommand(ctx, os.Args[1:])
	}
	if internalErr != nil {
		if !errors.Is(internalErr, context.Canceled) && !errors.Is(internalErr, flag.ErrHelp) {
			_, _ = fmt.Fprintln(os.Stderr, "error:", internalErr)
			os.Exit(1)
		}
		return
	}
	if handled {
		return
	}
	if filepath.Base(os.Args[0]) != "idlectl" {
		_, _ = fmt.Fprintf(os.Stderr, "unsupported executable name %q: this binary must be invoked as idlectl; the internal service names are reserved for installed launchd services\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		_, _ = fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	handled, err := runPublicCommand(ctx, os.Args[1], os.Args[2:])
	if !handled {
		_, _ = fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, pflag.ErrHelp) {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		if isUsageError(err) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func runPublicCommand(ctx context.Context, command string, args []string) (bool, error) {
	switch command {
	case "join":
		return true, runJoin(ctx, args)
	case "run":
		return true, runWorkload(ctx, args)
	case "recipe":
		return true, runRecipe(args, os.Stdin, os.Stdout)
	case "get":
		return true, runGet(ctx, args)
	case "logs":
		return true, runLogs(ctx, args)
	case "delete":
		return true, runDelete(ctx, args)
	case "create":
		return true, runCreateWorker(ctx, args)
	case "start":
		return true, runStartWorker(ctx, args)
	case "stop":
		return true, runStopWorker(ctx, args)
	case "load":
		return true, runLoadImage(ctx, args)
	case "status":
		return true, runStatus(ctx, args, os.Stdout)
	case "worker":
		return true, usagef(`worker subcommands moved: use "idlectl create worker NAME", "idlectl start|stop worker", "idlectl delete worker NAME", "idlectl status" (see idlectl help)`)
	case "version":
		fmt.Println(versionText())
		return true, nil
	case "help", "-h", "--help":
		return true, runHelp(ctx, args)
	default:
		return false, nil
	}
}

// runHelp prints the general usage, or routes "idlectl help COMMAND" to that
// command's --help output.
func runHelp(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usageText)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	}
	handled, err := runPublicCommand(ctx, args[0], []string{"--help"})
	if !handled {
		return usagef("unknown help topic %q\n%s", args[0], strings.TrimSuffix(usageText, "\n"))
	}
	return err
}

func versionText() string {
	return fmt.Sprintf("idlectl %s (%s, %s, %s/%s)", version, commit, buildDate, runtime.GOOS, runtime.GOARCH)
}

func runInternalBinary(ctx context.Context, binary string, args []string) (bool, error) {
	if strings.HasPrefix(binary, "io.idleloom.link.") {
		return true, runLink(ctx, args)
	}
	switch binary {
	case "idleloom-controller":
		return true, runController(ctx, args)
	case "idleloom-agent":
		return true, runAgent(ctx, args)
	case "idleloom-link":
		return true, runLink(ctx, args)
	case "idleloom-projection":
		return true, runProjection(ctx, args)
	default:
		return false, nil
	}
}

func runInternalCommand(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "internal" {
		return false, nil
	}
	if len(args) < 2 {
		return true, fmt.Errorf("internal service role is required")
	}
	switch args[1] {
	case "controller":
		return true, runController(ctx, args[2:])
	case "agent":
		return true, runAgent(ctx, args[2:])
	case "link":
		return true, runLink(ctx, args[2:])
	case "projection":
		return true, runProjection(ctx, args[2:])
	case "maintain":
		return true, runMaintain(ctx, args[2:])
	default:
		return true, fmt.Errorf("unknown internal service role %q", args[1])
	}
}

// runMaintain runs the detached worker certificate maintainer. It is spawned
// by the CLI itself (see maintainerCommandArgs) and hidden from public help.
func runMaintain(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("maintain", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	statePath := flags.String("state", "", "Idleloom state file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return idleloom.NewApp(os.Stdout, os.Stderr).Maintain(ctx, *statePath)
}

func runJoin(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("join")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private controller and agent state")
	runtimeRoot := flags.String("root", devruntime.DefaultRoot(), "native runtime and assignment root")
	tokenDuration := flags.Duration("token-duration", 8*time.Hour, "restricted credential lifetime, at most 24h")
	allowTOFU := flags.Bool("allow-tofu", false, "pin the currently observed API certificate when the source kubeconfig is insecure")
	resetTrust := flags.Bool("reset-trust", false, "replace the persisted API certificate identity after manual verification")
	forceConflicts := flags.Bool("force-conflicts", false, "take ownership of conflicting cluster API fields")
	link := flags.String("link", nativewirekube.ConnectivityWireKube, "cluster link: api-only or wirekube")
	yes := flags.Bool("yes", false, "approve the displayed join plan without prompting")
	installDependencies := flags.Bool("install-dependencies", false, "install missing cluster dependencies in non-interactive mode")
	shellAccess := flags.String("shell-access", "sandboxed", "maximum remote shell access: disabled, sandboxed, or host")
	projectionEnabled := flags.Bool("projection", true, "publish ephemeral Kubernetes Nodes and Pods with logs support")
	kubeletClientCNs := flags.String("kubelet-client-cn", "kube-apiserver-kubelet-client", "comma-separated kubelet client certificate common names")
	kubeletClientOrganizations := flags.String("kubelet-client-organization", "system:masters", "comma-separated kubelet client certificate organizations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return usagef("usage: idlectl join HOST [flags]")
	}
	hostID := enroll.NormalizeHostID(flags.Args()[0])
	if hostID == "" {
		return fmt.Errorf("the host ID must contain a letter or digit")
	}
	if installed, err := serviceinstall.HasReceipt(*stateDirectory); err != nil {
		return err
	} else if installed {
		enrolledHostID := hostID
		if identity, identityErr := enroll.IdentityFromState(*stateDirectory); identityErr == nil {
			enrolledHostID = identity.HostID
		}
		return fmt.Errorf("this Mac is already joined as host %q; run \"idlectl delete host %s\" before joining again", enrolledHostID, enrolledHostID)
	}
	publicBinary, err := serviceinstall.CaptureCurrentBinary()
	if err != nil {
		return fmt.Errorf("capture public native binary: %w", err)
	}
	resolvedShellAccess, err := parseShellAccess(*shellAccess)
	if err != nil {
		return err
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	credentialConfig, err := secureClusterConfig(ctx, config, *stateDirectory, *allowTOFU, *resetTrust)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(credentialConfig)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(credentialConfig)
	if err != nil {
		return err
	}
	if *link == nativewirekube.ConnectivityWireKube {
		if err := ensureWireKubeForJoin(ctx, wireKubeJoinConfig{
			Dynamic: dynamicClient, Kubeconfig: *kubeconfig, Context: *kubeContext,
			HostID: hostID, ShellAccess: resolvedShellAccess, Projection: *projectionEnabled,
			Yes: *yes, InstallDependencies: *installDependencies,
			Interactive: term.IsTerminal(int(os.Stdin.Fd())), Input: os.Stdin, Output: os.Stderr,
		}); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(os.Stderr, "installing Native API and restricted identities")
	if err := install.Apply(ctx, dynamicClient, *forceConflicts); err != nil {
		return err
	}
	if *projectionEnabled {
		_, _ = fmt.Fprintln(os.Stderr, "installing projection RBAC and admission policy")
		if err := install.ApplyProjection(ctx, dynamicClient, *forceConflicts); err != nil {
			return err
		}
	}
	if err := waitForNativeAPI(ctx, dynamicClient); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "installing locked Native model catalog")
	if err := install.ApplyCatalog(ctx, dynamicClient, *forceConflicts); err != nil {
		return err
	}
	result, err := enroll.Run(ctx, enroll.Config{
		REST: credentialConfig, Dynamic: dynamicClient, Kubernetes: clientset, HostID: hostID,
		StateDirectory: *stateDirectory, TokenDuration: *tokenDuration, Connectivity: *link,
		ShellAccess: resolvedShellAccess,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "enrolled host; installing launchd services")
	projectionKubeconfig := ""
	if *projectionEnabled {
		projectionKubeconfig = filepath.Join(*stateDirectory, "projection.kubeconfig")
		if _, err := enroll.WriteServiceKubeconfig(ctx, credentialConfig, clientset, "idleloom-system", "idleloom-projection", projectionKubeconfig, "idleloom-projection", *tokenDuration); err != nil {
			rollbackErr := rollbackJoin(context.Background(), dynamicClient, clientset, *stateDirectory, result.Namespace)
			return errors.Join(fmt.Errorf("create projection credential: %w", err), rollbackErr)
		}
	}
	if _, err := serviceinstall.Install(ctx, serviceinstall.Config{
		HostID: hostID, StateDirectory: *stateDirectory, RuntimeRoot: *runtimeRoot,
		Namespace: result.Namespace, AgentID: result.AgentID, LinkMode: result.Connectivity,
		ControllerKubeconfig: result.ControllerKubeconfig, AgentKubeconfig: result.AgentKubeconfig,
		LinkKubeconfig: result.LinkKubeconfig, ProjectionKubeconfig: projectionKubeconfig, PublicBinary: publicBinary,
		KubeletClientCommonNames: *kubeletClientCNs, KubeletClientOrganizations: *kubeletClientOrganizations,
	}); err != nil {
		rollbackErr := rollbackJoin(context.Background(), dynamicClient, clientset, *stateDirectory, result.Namespace)
		return errors.Join(fmt.Errorf("install native services: %w", err), rollbackErr)
	}
	// Exec-plugin credentials may expire while sudo waits for user input.
	readyClient, _, err := loadClusterClients(ctx, *kubeconfig, *kubeContext, *stateDirectory, *allowTOFU, *resetTrust)
	if err != nil {
		return fmt.Errorf("refresh cluster credentials after installing native services: %w", err)
	}
	_, _ = fmt.Fprintln(os.Stderr, "waiting for host readiness")
	if err := waitForHostReady(ctx, readyClient, result.Namespace, result.Connectivity == nativewirekube.ConnectivityWireKube, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for joined host: %w", err)
	}
	fmt.Printf("host/%s joined\n", hostID)
	fmt.Printf("namespace: %s\n", result.Namespace)
	fmt.Printf("link: %s\n", result.Connectivity)
	fmt.Printf("shell access: %s\n", result.ShellAccess)
	fmt.Printf("credentials expire: %s\n", result.ExpiresAt.Format(time.RFC3339))
	fmt.Printf("status: idlectl get host/%s\n", hostID)
	return nil
}

type wireKubeJoinConfig struct {
	Dynamic             dynamic.Interface
	Kubeconfig          string
	Context             string
	HostID              string
	ShellAccess         string
	Projection          bool
	Yes                 bool
	InstallDependencies bool
	Interactive         bool
	Input               io.Reader
	Output              io.Writer
}

func ensureWireKubeForJoin(ctx context.Context, config wireKubeJoinConfig) error {
	if config.Dynamic == nil {
		return fmt.Errorf("dynamic Kubernetes client is required")
	}
	if config.Input == nil {
		config.Input = strings.NewReader("")
	}
	if config.Output == nil {
		config.Output = io.Discard
	}
	_, err := config.Dynamic.Resource(nativewirekube.MeshesGVR).Get(ctx, "default", metav1.GetOptions{})
	if err == nil {
		report, inspectErr := nativewirekube.Inspect(ctx, config.Dynamic)
		if inspectErr != nil {
			return fmt.Errorf("existing WireKube installation is incompatible: %w", inspectErr)
		}
		_, _ = fmt.Fprintf(config.Output, "using existing WireKube mesh %s (%s)\n", report.MeshName, report.MeshCIDR)
		for _, warning := range report.Warnings {
			_, _ = fmt.Fprintln(config.Output, "warning:", warning)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("inspect WireKube installation: %w", err)
	}
	if config.Yes && !config.InstallDependencies {
		return fmt.Errorf("the WireKube dependency is not installed; rerun with --install-dependencies --yes, or use --link api-only")
	}
	if !config.Interactive && (!config.Yes || !config.InstallDependencies) {
		return fmt.Errorf("the WireKube dependency is not installed and input is non-interactive; rerun with --install-dependencies --yes, or use --link api-only")
	}

	_, _ = fmt.Fprintf(config.Output, "WireKube is not installed; resolving compatible release %s\n", wirekubecli.CompatibleVersion)
	lifecycle, err := newWireKubeLifecycle(ctx, config.Kubeconfig, config.Context)
	if err != nil {
		return fmt.Errorf("prepare WireKube installer: %w", err)
	}
	plan, err := lifecycle.Plan(ctx)
	if err != nil {
		return fmt.Errorf("plan WireKube installation: %w", err)
	}
	writeWireKubeJoinPlan(config.Output, config, plan)
	if !config.Yes {
		confirmed, err := confirmDefaultYes(config.Input, config.Output, "Install WireKube and join this host? [Y/n] ")
		if err != nil {
			return err
		}
		if !confirmed {
			return fmt.Errorf("join cancelled; rerun with --link api-only to join without inbound Kubernetes connectivity")
		}
	}
	_, _ = fmt.Fprintln(config.Output, "installing WireKube cluster dependencies")
	result, err := lifecycle.Install(ctx, plan)
	if err != nil {
		return fmt.Errorf("install WireKube: %w", err)
	}
	_, _ = fmt.Fprintf(config.Output, "WireKube installation %s is ready; continuing host enrollment\n", result.InstallationID)
	return nil
}

func writeWireKubeJoinPlan(out io.Writer, config wireKubeJoinConfig, plan wirekubecli.Plan) {
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "Idleloom connected-host plan")
	_, _ = fmt.Fprintf(out, "  Host:          %s\n", config.HostID)
	_, _ = fmt.Fprintf(out, "  Shell access:  %s\n", config.ShellAccess)
	_, _ = fmt.Fprintf(out, "  Projection:    %t\n", config.Projection)
	_, _ = fmt.Fprintf(out, "  Cluster:       %s\n", plan.Context)
	_, _ = fmt.Fprintf(out, "  Kubernetes:    %s\n", plan.Detection.KubernetesVersion)
	_, _ = fmt.Fprintf(out, "  CNI:           %s\n", plan.Detection.CNI)
	_, _ = fmt.Fprintf(out, "  WireKube:      %s\n", plan.WireKubeVersion)
	_, _ = fmt.Fprintf(out, "  Mesh CIDR:     %s\n", plan.MeshCIDR)
	_, _ = fmt.Fprintf(out, "  Image:         %s\n", plan.Image)
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out, "Infrastructure impact")
	for _, impact := range plan.Impact {
		_, _ = fmt.Fprintf(out, "  - %s\n", impact)
	}
	for _, warning := range plan.Warnings {
		_, _ = fmt.Fprintf(out, "  warning: %s\n", warning)
	}
}

func confirmDefaultYes(in io.Reader, out io.Writer, prompt string) (bool, error) {
	_, _ = fmt.Fprint(out, prompt)
	response, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && len(response) == 0 {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("answer yes or no")
	}
}

func rollbackJoin(ctx context.Context, dynamicClient dynamic.Interface, clientset kubernetes.Interface, stateDirectory, namespace string) error {
	var errs []error
	if err := serviceinstall.Remove(ctx, stateDirectory); err != nil {
		errs = append(errs, fmt.Errorf("remove partial services: %w", err))
	}
	if hasState, err := nativewirekube.HasState(stateDirectory); err != nil {
		errs = append(errs, err)
	} else if hasState {
		if err := nativewirekube.Revoke(ctx, nativewirekube.RevokeConfig{
			Dynamic: dynamicClient, StateDirectory: stateDirectory, WaitTimeout: time.Minute, Force: true,
		}); err != nil {
			errs = append(errs, fmt.Errorf("revoke partial host link: %w", err))
		}
	}
	if err := clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete partial host namespace: %w", err))
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(stateDirectory); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func waitForHostReady(ctx context.Context, client dynamic.Interface, namespace string, requireConnected bool, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		object, err := client.Resource(nativekube.HostsGVR).Namespace(namespace).Get(ctx, "host", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		var host nativev1alpha1.IdleloomHost
		if err := nativekube.FromUnstructured(object, &host); err != nil {
			return false, err
		}
		ready := conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionReady) == string(metav1.ConditionTrue)
		connected := conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionConnected) == string(metav1.ConditionTrue)
		return ready && (!requireConnected || connected), nil
	})
}

func parseShellAccess(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "disabled":
		return nativev1alpha1.ShellAccessDisabled, nil
	case "sandboxed":
		return nativev1alpha1.ShellAccessSandboxed, nil
	case "host":
		return nativev1alpha1.ShellAccessHost, nil
	default:
		return "", fmt.Errorf("--shell-access must be disabled, sandboxed, or host")
	}
}

func runLink(ctx context.Context, args []string) (returnErr error) {
	flags := flag.NewFlagSet("link", flag.ContinueOnError)
	stateDirectory := flags.String("state-dir", "", "enrolled private state directory")
	kubeconfig := flags.String("kubeconfig", "", "restricted WireKube peer kubeconfig")
	interval := flags.Duration("interval", 5*time.Second, "runtime status update interval")
	enabled := flags.Bool("enable-wirekube-connected-leaf", false, "run the development-only privileged WireKube leaf tunnel")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*enabled {
		return fmt.Errorf("connected leaf is an alpha feature; rerun with --enable-wirekube-connected-leaf")
	}
	if *stateDirectory == "" {
		return fmt.Errorf("--state-dir is required")
	}
	if *interval <= 0 || *interval > 10*time.Second {
		return fmt.Errorf("--interval must be positive and at most 10s")
	}
	if runtime.GOOS == "darwin" && os.Geteuid() != 0 {
		return fmt.Errorf("link service must run as root")
	}
	state, err := nativewirekube.ReadState(*stateDirectory)
	if err != nil {
		return err
	}
	if state.PeerNamespace == "" || state.PeerServiceAccount == "" || state.LinkKubeconfig == "" {
		return fmt.Errorf("legacy WireKube external-peer enrollment is not supported by this link service; delete and rejoin the host")
	}
	if *kubeconfig == "" {
		return fmt.Errorf("--kubeconfig is required for the WireKube peer link")
	}
	clusterConfig, err := loadConfig(*kubeconfig, "")
	if err != nil {
		return err
	}
	clusterConfig, err = credential.Configure(clusterConfig, credential.Options{
		Namespace: state.PeerNamespace, ServiceAccount: state.PeerServiceAccount,
		KubeconfigPath: *kubeconfig, TokenDuration: 8 * time.Hour,
		Logf: func(format string, values ...any) { _, _ = fmt.Fprintf(os.Stderr, "link: "+format+"\n", values...) },
	})
	if err != nil {
		return fmt.Errorf("configure WireKube peer credential: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(clusterConfig)
	if err != nil {
		return err
	}
	kubernetesClient, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		return err
	}
	relayTokenExpiry := time.Time{}
	refreshRelayToken := func(force bool) error {
		if state.RelayTransport != "wss" {
			return nil
		}
		if !force && time.Now().Before(relayTokenExpiry.Add(-15*time.Minute)) {
			return nil
		}
		expires, err := nativewirekube.WriteRelayToken(ctx, kubernetesClient, state, *stateDirectory, time.Hour)
		if err != nil {
			return err
		}
		relayTokenExpiry = expires
		return nil
	}
	if err := refreshRelayToken(true); err != nil {
		return err
	}
	runtimeDirectory, err := nativewirekube.DefaultRuntimeDirectory(state)
	if err != nil {
		return err
	}
	lock, err := nativewirekube.AcquireRuntimeLock(runtimeDirectory)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, lock.Close()) }()
	startingStatus := nativewirekube.RuntimeStatus{
		Version: nativewirekube.RuntimeStatusVersion, InstanceID: lock.InstanceID, ProcessID: os.Getpid(),
		PeerUID: state.PeerUID, ObservedAt: time.Now().UTC(), Error: "WireKube tunnel is starting",
	}
	if err := nativewirekube.WriteRuntimeStatus(runtimeDirectory, startingStatus); err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, nativewirekube.RemoveRuntimeStatus(runtimeDirectory, lock.InstanceID))
	}()
	tunnel, err := nativewirekube.StartTunnel(ctx, state, nativewirekube.TunnelConfig{
		RelayTokenFile: nativewirekube.RelayTokenPath(*stateDirectory),
	}, func(format string, values ...any) {
		_, _ = fmt.Fprintf(os.Stderr, "link: "+format+"\n", values...)
	})
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, tunnel.Close()) }()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := nativewirekube.UpdatePeerStatus(cleanupCtx, dynamicClient, state, nativewirekube.TunnelSnapshot{}, false); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "link: clear peer status: %v\n", err)
		}
	}()
	writeStatus := func() error {
		var statusErr error
		if err := refreshRelayToken(false); err != nil {
			if relayTokenExpiry.IsZero() || !time.Now().Before(relayTokenExpiry) {
				statusErr = errors.Join(statusErr, err)
			} else {
				_, _ = fmt.Fprintf(os.Stderr, "link: relay token refresh deferred: %v\n", err)
			}
		}
		if err := tunnel.SyncPeers(ctx, dynamicClient); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "link: peer synchronization deferred: %v\n", err)
		}
		if err := tunnel.Validate(ctx); err != nil {
			statusErr = errors.Join(statusErr, err)
		}
		snapshot, snapshotErr := tunnel.Snapshot()
		status := nativewirekube.RuntimeStatus{
			Version: nativewirekube.RuntimeStatusVersion, InstanceID: lock.InstanceID, ProcessID: os.Getpid(),
			PeerUID: state.PeerUID, InterfaceName: tunnel.InterfaceName(),
			LastHandshakeTime: snapshot.LastHandshake, BytesReceived: snapshot.BytesReceived,
			BytesSent: snapshot.BytesSent, ObservedAt: time.Now().UTC(),
		}
		statusErr = errors.Join(statusErr, snapshotErr)
		if statusErr != nil {
			status.Error = statusErr.Error()
		}
		if err := nativewirekube.WriteRuntimeStatus(runtimeDirectory, status); err != nil {
			return err
		}
		if err := nativewirekube.UpdatePeerStatus(ctx, dynamicClient, state, snapshot, statusErr == nil); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "link: %v\n", err)
		}
		return nil
	}
	if err := writeStatus(); err != nil {
		return err
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := writeStatus(); err != nil {
				return err
			}
		}
	}
}

func runController(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterFlags("controller")
	interval := flags.Duration("interval", 2*time.Second, "reconciliation interval")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *kubeconfig == "" {
		return fmt.Errorf("--kubeconfig is required; use the restricted controller kubeconfig created by join")
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	config.Timeout = 10 * time.Second
	config, err = credential.Configure(config, credential.Options{
		Namespace: "idleloom-system", ServiceAccount: "idleloom-controller", KubeconfigPath: *kubeconfig,
		Logf: func(format string, values ...any) {
			_, _ = fmt.Fprintf(os.Stderr, "controller: "+format+"\n", values...)
		},
	})
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	if err := verifyIdentity(ctx, clientset, "system:serviceaccount:idleloom-system:idleloom-controller"); err != nil {
		return err
	}
	reconciler := &nativecontroller.Reconciler{Dynamic: dynamicClient, Kubernetes: clientset, Coordination: clientset.CoordinationV1()}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	identity := hostname + "-" + string(uuid.NewUUID())
	return nativecontroller.RunLeaderElected(ctx, clientset.CoordinationV1(), nativecontroller.LeaderOptions{
		Namespace: "idleloom-system", Name: "idleloom-controller-leader", Identity: identity,
		LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		Reconcile: func(leaderCtx context.Context) error {
			return runControllerLoop(leaderCtx, reconciler, *interval)
		},
		Logf: func(format string, values ...any) {
			_, _ = fmt.Fprintf(os.Stderr, "controller: "+format+"\n", values...)
		},
	})
}

func runControllerLoop(ctx context.Context, reconciler *nativecontroller.Reconciler, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("reconciliation interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := reconciler.ReconcileOnce(ctx); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "controller:", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runProjection(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterFlags("projection")
	enabled := flags.Bool("enable-kubernetes-projection", false, "enable the observability-only alpha Node and Pod projection")
	inCluster := flags.Bool("in-cluster", false, "use the pod service account instead of a kubeconfig")
	interval := flags.Duration("interval", 2*time.Second, "projection reconciliation interval")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private cluster trust state for external kubeconfigs")
	allowTOFU := flags.Bool("allow-tofu", false, "pin the currently observed API certificate when an external kubeconfig is insecure")
	resetTrust := flags.Bool("reset-trust", false, "replace the persisted API certificate identity after manual verification")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*enabled {
		return fmt.Errorf("projection is an observability-only alpha feature; rerun with --enable-kubernetes-projection")
	}
	if *interval <= 0 {
		return fmt.Errorf("projection reconciliation interval must be positive")
	}
	var config *rest.Config
	var err error
	if *inCluster {
		if *kubeconfig != "" || *kubeContext != "" || *allowTOFU || *resetTrust {
			return fmt.Errorf("--in-cluster cannot be combined with external kubeconfig or TOFU flags")
		}
		config, err = rest.InClusterConfig()
		if err != nil {
			return err
		}
		config.UserAgent = "idleloom-projection"
	} else {
		config, err = loadConfig(*kubeconfig, *kubeContext)
		if err != nil {
			return err
		}
		config, err = secureClusterConfig(ctx, config, *stateDirectory, *allowTOFU, *resetTrust)
		if err != nil {
			return err
		}
		config, err = credential.Configure(config, credential.Options{
			Namespace: "idleloom-system", ServiceAccount: "idleloom-projection", KubeconfigPath: *kubeconfig,
			Logf: func(format string, values ...any) {
				_, _ = fmt.Fprintf(os.Stderr, "projection: "+format+"\n", values...)
			},
		})
		if err != nil {
			return err
		}
	}
	config.Timeout = 10 * time.Second
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	if !*inCluster {
		if err := verifyRestrictedIdentity(ctx, clientset, "system:serviceaccount:idleloom-system:idleloom-projection", "idleloom-system"); err != nil {
			return err
		}
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	projector := &nativeprojection.Controller{
		Dynamic: dynamicClient, Kubernetes: clientset,
	}
	identity := hostname + "-" + string(uuid.NewUUID())
	return nativecontroller.RunLeaderElected(ctx, clientset.CoordinationV1(), nativecontroller.LeaderOptions{
		Namespace: "idleloom-system", Name: "idleloom-projection-leader", Identity: identity,
		LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second,
		Reconcile: func(leaderCtx context.Context) error {
			return runProjectionLoop(leaderCtx, projector, *interval)
		},
		Logf: func(format string, values ...any) {
			_, _ = fmt.Fprintf(os.Stderr, "projection: "+format+"\n", values...)
		},
	})
}

func runProjectionLoop(ctx context.Context, projector *nativeprojection.Controller, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := projector.ReconcileOnce(ctx); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "projection:", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runAgent(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterFlags("agent")
	namespace := flags.String("namespace", "", "enrolled host namespace")
	agentID := flags.String("agent-id", "", "enrolled agent identity")
	root := flags.String("root", devruntime.DefaultRoot(), "prepared runtime root")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private agent state")
	listen := flags.String("listen", "127.0.0.1:0", "loopback endpoint")
	link := flags.String("link", nativewirekube.ConnectivityAPIOnly, "cluster link: api-only or wirekube")
	kubeletClientCNs := flags.String("kubelet-client-cn", "kube-apiserver-kubelet-client", "comma-separated kubelet client certificate common names")
	kubeletClientOrganizations := flags.String("kubelet-client-organization", "system:masters", "comma-separated kubelet client certificate organizations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *namespace == "" || *agentID == "" {
		return fmt.Errorf("--namespace and --agent-id are required")
	}
	if *kubeconfig == "" {
		return fmt.Errorf("--kubeconfig is required; use the host-scoped agent kubeconfig created by join")
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	config.Timeout = 10 * time.Second
	config, err = credential.Configure(config, credential.Options{
		Namespace: *namespace, ServiceAccount: "idleloom-agent", KubeconfigPath: *kubeconfig,
		Logf: func(format string, values ...any) { _, _ = fmt.Fprintf(os.Stderr, "agent: "+format+"\n", values...) },
	})
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	expectedUser := "system:serviceaccount:" + *namespace + ":idleloom-agent"
	if err := verifyRestrictedIdentity(ctx, clientset, expectedUser, *namespace); err != nil {
		return err
	}
	var connectivityStatus func() (nativev1alpha1.HostConnectivityStatus, error)
	var kubeletBridge *nativeagent.KubeletBridgeConfig
	serveListenAddress := ""
	if *link == nativewirekube.ConnectivityWireKube {
		state, err := nativewirekube.ReadState(*stateDirectory)
		if err != nil {
			return fmt.Errorf("read WireKube connected leaf: %w", err)
		}
		runtimeDirectory, err := nativewirekube.DefaultRuntimeDirectory(state)
		if err != nil {
			return err
		}
		connectivityStatus = func() (nativev1alpha1.HostConnectivityStatus, error) {
			return nativewirekube.ReadRuntimeStatus(runtimeDirectory, state, time.Now().UTC(), 15*time.Second)
		}
		meshIP, err := assignedMeshIPv4(state.AssignedMeshIP)
		if err != nil {
			return err
		}
		serveListenAddress = net.JoinHostPort(meshIP, fmt.Sprint(nativev1alpha1.NativeServingPort))
		clientCA, err := os.ReadFile(kubeletbridge.ClientCAPath(*stateDirectory))
		if err != nil {
			return fmt.Errorf("read enrolled Kubernetes client CA: %w", err)
		}
		kubeletBridge = &nativeagent.KubeletBridgeConfig{
			ListenAddress: "0.0.0.0:10250", Identity: kubeletbridge.IdentityPaths(*stateDirectory), ClientCA: clientCA,
			AllowedClientCommonNames: splitNonEmpty(*kubeletClientCNs), AllowedClientOrganizations: splitNonEmpty(*kubeletClientOrganizations),
		}
	} else if *link != nativewirekube.ConnectivityAPIOnly {
		return fmt.Errorf("--link must be %q or %q", nativewirekube.ConnectivityAPIOnly, nativewirekube.ConnectivityWireKube)
	}
	agent, err := nativeagent.NewDevAgent(nativeagent.DevAgentConfig{
		Dynamic: dynamicClient, Kubernetes: clientset, Namespace: *namespace, AgentID: *agentID,
		Layout: devruntime.NewLayout(*root), StateDirectory: *stateDirectory,
		KubeconfigPath: *kubeconfig, ListenAddress: *listen, ServeListenAddress: serveListenAddress,
		ConnectivityStatus: connectivityStatus,
		KubeletBridge:      kubeletBridge,
		Logf:               func(format string, values ...any) { _, _ = fmt.Fprintf(os.Stderr, "agent: "+format+"\n", values...) },
	})
	if err != nil {
		return err
	}
	return agent.Run(ctx)
}

func splitNonEmpty(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func assignedMeshIPv4(value string) (string, error) {
	value = strings.TrimSpace(value)
	if ip := net.ParseIP(value); ip != nil && ip.To4() != nil {
		return ip.String(), nil
	}
	ip, network, err := net.ParseCIDR(value)
	if err != nil || ip.To4() == nil {
		return "", fmt.Errorf("the WireKube connected leaf has no valid assigned IPv4 address")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 32 {
		return "", fmt.Errorf("the WireKube connected leaf must have a single assigned IPv4 address")
	}
	return ip.String(), nil
}

func runWorkload(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("run")
	namespace := flags.StringP("namespace", "n", "", "workload namespace; defaults to the current context")
	script := flags.String("shell", "", "shell script to execute")
	isolation := flags.String("isolation", "sandbox", "shell isolation: sandbox or host")
	network := flags.String("network", "", "shell network access: sandbox defaults to none; host requires outbound")
	timeout := flags.Duration("timeout", time.Hour, "shell execution timeout, at most 24h")
	memory := flags.String("memory", "1Gi", "unified memory reservation")
	experiment := flags.String("experiment", "", "run experiment label; defaults to NAME")
	attempt := flags.Int32("attempt", 1, "run attempt number")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return usagef("usage: idlectl run NAME --shell '<script>' [flags]")
	}
	name := flags.Args()[0]
	if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
		return fmt.Errorf("invalid workload name %q: %s", name, strings.Join(problems, "; "))
	}
	if strings.TrimSpace(*script) == "" {
		return fmt.Errorf("--shell is required")
	}
	if *experiment == "" {
		*experiment = name
	}
	if problems := validation.IsDNS1123Label(*experiment); len(problems) > 0 {
		return fmt.Errorf("invalid --experiment %q: %s", *experiment, strings.Join(problems, "; "))
	}
	if *attempt < 1 || *attempt > 1000 {
		return fmt.Errorf("--attempt must be between 1 and 1000")
	}
	resolvedIsolation, err := parseShellIsolation(*isolation)
	if err != nil {
		return err
	}
	resolvedNetwork, err := parseShellNetwork(*network, resolvedIsolation)
	if err != nil {
		return err
	}
	if *timeout < time.Second || *timeout > 24*time.Hour || *timeout%time.Second != 0 {
		return fmt.Errorf("--timeout must be a whole number of seconds between 1s and 24h")
	}
	request, err := resource.ParseQuantity(*memory)
	if err != nil || request.Sign() <= 0 {
		return fmt.Errorf("--memory must be a positive Kubernetes quantity")
	}
	resolvedNamespace, err := resolveNamespace(*kubeconfig, *kubeContext, *namespace)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "warning: shell commands are stored in Kubernetes API objects; do not include secrets")
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: resolvedNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "idleloom",
				"ai.idleloom.io/run":           name, "ai.idleloom.io/task": "shell",
				"ai.idleloom.io/experiment": *experiment, "ai.idleloom.io/attempt": fmt.Sprint(*attempt),
			},
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode: nativev1alpha1.WorkloadModeShell,
			Shell: &nativev1alpha1.WorkloadShell{
				Script: *script, Isolation: resolvedIsolation, Network: resolvedNetwork,
				TimeoutSeconds: int32(*timeout / time.Second),
			},
			Run:       &nativev1alpha1.WorkloadRunSpec{Task: "shell", Experiment: *experiment, Attempt: *attempt},
			Resources: nativev1alpha1.WorkloadResources{UnifiedMemoryRequest: request},
		},
	}
	if err := nativev1alpha1.ValidateWorkload(workload); err != nil {
		return err
	}
	object, err := nativekube.ToUnstructured(workload)
	if err != nil {
		return err
	}
	_, err = client.Resource(nativekube.WorkloadsGVR).Namespace(resolvedNamespace).Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("idleloomworkload.ai.idleloom.io/%s created\n", name)
	fmt.Printf("logs: idlectl logs -f -n %s workload/%s\n", resolvedNamespace, name)
	return nil
}

func runGet(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("get")
	namespace := flags.StringP("namespace", "n", "", "resource namespace; defaults to the current context")
	allNamespaces := flags.BoolP("all-namespaces", "A", false, "list resources across all namespaces")
	output := flags.StringP("output", "o", "table", "output format: table, json, or yaml")
	statePath := flags.String("state", "", workerStateHelp)
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	resourceName, name, err := parseResourceReference(positional, false)
	if err != nil {
		return usagef("usage: idlectl get (hosts|workers|workloads) [NAME] [flags]: %v", err)
	}
	if err := validateOutputFormat(*output); err != nil {
		return err
	}
	if *allNamespaces && name != "" {
		return fmt.Errorf("a resource cannot be retrieved by name across all namespaces")
	}
	if resourceName == resourceHosts && (*namespace != "" || *allNamespaces) {
		return fmt.Errorf("hosts are logical cluster-wide resources; do not use --namespace or --all-namespaces")
	}
	if flags.Changed("state") && resourceName != resourceWorkers {
		return usagef("--state only applies to workers")
	}
	if resourceName == resourceWorkers {
		if *namespace != "" || *allNamespaces {
			return fmt.Errorf("workers are local to this Mac; do not use --namespace or --all-namespaces")
		}
		return getWorkers(ctx, os.Stdout, *kubeconfig, *kubeContext, *statePath, name, *output)
	}
	resolvedNamespace, err := resolveNamespace(*kubeconfig, *kubeContext, *namespace)
	if err != nil {
		return err
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	switch resourceName {
	case resourceWorkloads:
		return getWorkloads(ctx, client, resolvedNamespace, *allNamespaces, name, *output)
	case resourceHosts:
		return getHosts(ctx, client, resolvedNamespace, *allNamespaces, name, *output)
	default:
		return fmt.Errorf("unsupported resource %q", resourceName)
	}
}

func getWorkloads(ctx context.Context, client dynamic.Interface, namespace string, allNamespaces bool, name, output string) error {
	resource := client.Resource(nativekube.WorkloadsGVR)
	if name != "" {
		object, err := resource.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var workload nativev1alpha1.IdleloomWorkload
		if err := nativekube.FromUnstructured(object, &workload); err != nil {
			return err
		}
		if output != "table" {
			return printStructured(workload, output)
		}
		return printWorkloadTable([]nativev1alpha1.IdleloomWorkload{workload}, false)
	}
	listNamespace := namespace
	if allNamespaces {
		listNamespace = metav1.NamespaceAll
	}
	objects, err := resource.Namespace(listNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	workloads := make([]nativev1alpha1.IdleloomWorkload, 0, len(objects.Items))
	for index := range objects.Items {
		var workload nativev1alpha1.IdleloomWorkload
		if err := nativekube.FromUnstructured(&objects.Items[index], &workload); err != nil {
			return err
		}
		workloads = append(workloads, workload)
	}
	if output != "table" {
		return printStructured(nativev1alpha1.IdleloomWorkloadList{
			TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadList"},
			ListMeta: metav1.ListMeta{ResourceVersion: objects.GetResourceVersion(), Continue: objects.GetContinue()},
			Items:    workloads,
		}, output)
	}
	return printWorkloadTable(workloads, allNamespaces)
}

func getHosts(ctx context.Context, client dynamic.Interface, namespace string, allNamespaces bool, name, output string) error {
	resource := client.Resource(nativekube.HostsGVR)
	if name != "" {
		hostID := enroll.NormalizeHostID(name)
		objects, err := resource.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: "ai.idleloom.io/host-id=" + hostID})
		if err != nil {
			return err
		}
		if len(objects.Items) == 0 {
			if workerName := localWorkerNodeName(); workerName != "" && (workerName == name || workerName == hostID) {
				return fmt.Errorf("%q is an Idleloom worker, not a Native Metal host; try \"idlectl get workers\"", name)
			}
			return apierrors.NewNotFound(nativekube.HostsGVR.GroupResource(), hostID)
		}
		if len(objects.Items) != 1 {
			return fmt.Errorf("host ID %q matched %d host mailboxes", hostID, len(objects.Items))
		}
		var host nativev1alpha1.IdleloomHost
		if err := nativekube.FromUnstructured(&objects.Items[0], &host); err != nil {
			return err
		}
		if output != "table" {
			return printStructured(host, output)
		}
		return printHostTable([]nativev1alpha1.IdleloomHost{host}, false)
	}
	objects, err := resource.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	hosts := make([]nativev1alpha1.IdleloomHost, 0, len(objects.Items))
	for index := range objects.Items {
		var host nativev1alpha1.IdleloomHost
		if err := nativekube.FromUnstructured(&objects.Items[index], &host); err != nil {
			return err
		}
		hosts = append(hosts, host)
	}
	if output != "table" {
		return printStructured(nativev1alpha1.IdleloomHostList{
			TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHostList"},
			ListMeta: metav1.ListMeta{ResourceVersion: objects.GetResourceVersion(), Continue: objects.GetContinue()},
			Items:    hosts,
		}, output)
	}
	return printHostTable(hosts, true)
}

func runLogs(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("logs")
	namespace := flags.StringP("namespace", "n", "", "workload namespace; defaults to the current context")
	follow := flags.BoolP("follow", "f", false, "follow the log stream")
	local := flags.Bool("local", false, "read logs from this joined Mac instead of the kubelet bridge")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private enrolled host state used with --local")
	tail := flags.Int64("tail", -1, "number of recent lines to show")
	since := flags.Duration("since", 0, "only return logs newer than this duration")
	timestamps := flags.Bool("timestamps", false, "include timestamps")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resourceName, name, err := parseResourceReference(flags.Args(), true)
	if err != nil {
		return usagef("usage: idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]: %v", err)
	}
	if resourceName != resourceWorkloads {
		return fmt.Errorf("logs supports workloads only")
	}
	if *tail < -1 {
		return fmt.Errorf("--tail must be -1 or a non-negative number")
	}
	if *since < 0 || (*since > 0 && *since < time.Second) {
		return fmt.Errorf("--since must be zero or at least 1s")
	}
	if *local && *follow {
		return fmt.Errorf("--local currently supports completed snapshots only; do not use --follow")
	}
	resolvedNamespace, err := resolveNamespace(*kubeconfig, *kubeContext, *namespace)
	if err != nil {
		return err
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	object, err := dynamicClient.Resource(nativekube.WorkloadsGVR).Namespace(resolvedNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var workload nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &workload); err != nil {
		return err
	}
	if *local {
		if workload.Status.AssignmentRef == nil || workload.Status.AssignmentRef.UID == "" {
			return fmt.Errorf("workload %s/%s has no assignment yet", resolvedNamespace, workload.Name)
		}
		var sinceTime time.Time
		if *since > 0 {
			sinceTime = time.Now().Add(-*since)
		}
		entries, err := kubeletbridge.ReadLogSnapshot(
			filepath.Join(*stateDirectory, "container-logs.jsonl"), string(workload.Status.AssignmentRef.UID), 1<<20, sinceTime, *tail,
		)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if *timestamps {
				fmt.Printf("%s ", entry.Time.Format(time.RFC3339Nano))
			}
			fmt.Println(entry.Message)
		}
		return nil
	}
	podName, err := waitForProjectedLogEndpoint(ctx, clientset, resolvedNamespace, workload.Name, workload.UID, 30*time.Second)
	if err != nil {
		return err
	}
	options := &corev1.PodLogOptions{Container: "native-metal", Follow: *follow, Timestamps: *timestamps}
	if *tail >= 0 {
		options.TailLines = tail
	}
	if *since > 0 {
		seconds := int64(*since / time.Second)
		options.SinceSeconds = &seconds
	}
	stream, err := clientset.CoreV1().Pods(resolvedNamespace).GetLogs(podName, options).Stream(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = io.Copy(os.Stdout, stream)
	return err
}

func waitForProjectedLogEndpoint(ctx context.Context, client kubernetes.Interface, namespace, workloadName string, workloadUID types.UID, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("projected log endpoint wait must be positive")
	}
	selector := nativeprojection.LabelWorkloadUID + "=" + string(workloadUID)
	lastState := "projected Pod has not been created"
	var podName string
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 1 {
			return false, fmt.Errorf("workload %s/%s has %d projected Pods", namespace, workloadName, len(pods.Items))
		}
		if len(pods.Items) == 0 {
			lastState = "projected Pod has not been created"
			return false, nil
		}
		pod := &pods.Items[0]
		podName = pod.Name
		if pod.Annotations[nativeprojection.AnnotationLogs] != "supported" || pod.Annotations[nativeprojection.AnnotationKubeletAPI] != "logs-only" {
			lastState = "projection log probe has not succeeded"
			return false, nil
		}
		if pod.Spec.NodeName == "" {
			lastState = "projected Pod has no Node"
			return false, nil
		}
		node, err := client.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				lastState = "projected Node has not been created"
				return false, nil
			}
			return false, err
		}
		if node.Status.DaemonEndpoints.KubeletEndpoint.Port == 0 || preferredNodeAddress(node.Status.Addresses) == "" {
			lastState = "projected Node log address has not converged"
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("projected log endpoint for workload %s/%s did not become ready within %s: %s", namespace, workloadName, timeout, lastState)
		}
		return "", err
	}
	return podName, nil
}

func preferredNodeAddress(addresses []corev1.NodeAddress) string {
	for _, addressType := range []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP, corev1.NodeHostName} {
		for _, address := range addresses {
			if address.Type == addressType && strings.TrimSpace(address.Address) != "" {
				return address.Address
			}
		}
	}
	return ""
}

func runDelete(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("delete")
	namespace := flags.StringP("namespace", "n", "", "resource namespace; defaults to the current context")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private enrolled host state")
	allowTOFU := flags.Bool("allow-tofu", false, "pin the currently observed API certificate when the source kubeconfig is insecure")
	resetTrust := flags.Bool("reset-trust", false, "replace the persisted API certificate identity after manual verification")
	waitForDeletion := flags.Bool("wait", true, "wait for the workload and its assignment to be removed")
	timeout := flags.Duration("timeout", time.Minute, "time to wait for deletion")
	statePath := flags.String("state", "", workerStateHelp)
	force := flags.Bool("force", false, "delete the worker even when workload Pods are active")
	localOnly := flags.Bool("local-only", false, "delete local worker VM state without contacting Kubernetes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resourceName, name, err := parseResourceReference(flags.Args(), false)
	if err != nil {
		return usagef("usage: idlectl delete ((host|worker|workload) NAME | (host|worker|workload)/NAME) [flags]: %v", err)
	}
	if resourceName == resourceWorkers {
		for _, flagName := range []string{"namespace", "state-dir", "allow-tofu", "reset-trust", "wait", "timeout", "kubeconfig", "context"} {
			if flags.Changed(flagName) {
				return usagef("--%s does not apply to workers", flagName)
			}
		}
		return deleteWorker(ctx, *statePath, name, *force, *localOnly)
	}
	for _, flagName := range []string{"state", "force", "local-only"} {
		if flags.Changed(flagName) {
			return usagef("--%s only applies to workers", flagName)
		}
	}
	if name == "" {
		return usagef("usage: idlectl delete ((host|worker|workload) NAME | (host|worker|workload)/NAME) [flags]")
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if resourceName == resourceHosts {
		if *namespace != "" {
			return fmt.Errorf("hosts are logical cluster-wide resources; do not use --namespace")
		}
		if flags.Changed("wait") && !*waitForDeletion {
			return fmt.Errorf("host deletion always waits for safe cleanup; --wait=false is not supported")
		}
		return deleteHost(ctx, *kubeconfig, *kubeContext, *stateDirectory, name, *allowTOFU, *resetTrust, *timeout)
	}
	if resourceName != resourceWorkloads {
		return fmt.Errorf("deleting %s is not supported", resourceName)
	}
	resolvedNamespace, err := resolveNamespace(*kubeconfig, *kubeContext, *namespace)
	if err != nil {
		return err
	}
	config, err := loadConfig(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	if err := client.Resource(nativekube.WorkloadsGVR).Namespace(resolvedNamespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return err
	}
	if *waitForDeletion {
		err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, *timeout, true, func(ctx context.Context) (bool, error) {
			_, err := client.Resource(nativekube.WorkloadsGVR).Namespace(resolvedNamespace).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			return fmt.Errorf("wait for workload deletion: %w", err)
		}
	}
	fmt.Printf("idleloomworkload.ai.idleloom.io/%s deleted\n", name)
	return nil
}

func deleteHost(ctx context.Context, kubeconfig, kubeContext, stateDirectory, requestedHostID string, allowTOFU, resetTrust bool, timeout time.Duration) error {
	identity, err := enroll.IdentityFromState(stateDirectory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if workerName := localWorkerNodeName(); workerName != "" && (workerName == requestedHostID || workerName == enroll.NormalizeHostID(requestedHostID)) {
				return fmt.Errorf("no Native Metal host is enrolled on this Mac; %q is an Idleloom worker — use \"idlectl delete worker %s\"", requestedHostID, workerName)
			}
			return fmt.Errorf("no Native Metal host is enrolled on this Mac, so there is nothing to delete. Workers are removed with \"idlectl delete worker NAME\".")
		}
		return fmt.Errorf("read enrolled host identity: %w", err)
	}
	stateHostID := identity.HostID
	if enroll.NormalizeHostID(requestedHostID) != stateHostID {
		return fmt.Errorf("this Mac is joined as host %q, not %q; run \"idlectl delete host %s\"", stateHostID, requestedHostID, stateHostID)
	}
	dynamicClient, clientset, err := loadClusterClients(ctx, kubeconfig, kubeContext, stateDirectory, allowTOFU, resetTrust)
	if err != nil {
		return err
	}
	namespace := "idleloom-host-" + stateHostID
	namespaceObject, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("verify host namespace in selected cluster: %w", err)
	}
	if !namespaceOwnedByEnrollment(namespaceObject, identity) {
		return fmt.Errorf("selected cluster namespace %s does not belong to this local enrollment", namespace)
	}
	if err := ensureHostUnused(ctx, dynamicClient, namespace); err != nil {
		return err
	}
	if err := serviceinstall.Remove(ctx, stateDirectory); err != nil {
		return fmt.Errorf("remove native services: %w", err)
	}
	// Exec-plugin credentials may expire while sudo waits for user input.
	dynamicClient, clientset, err = loadClusterClients(ctx, kubeconfig, kubeContext, stateDirectory, allowTOFU, resetTrust)
	if err != nil {
		return fmt.Errorf("refresh cluster credentials after removing native services: %w", err)
	}
	namespaceObject, err = clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reverify host namespace in selected cluster: %w", err)
	}
	if !namespaceOwnedByEnrollment(namespaceObject, identity) {
		return fmt.Errorf("selected cluster namespace %s no longer belongs to this local enrollment", namespace)
	}
	if err := ensureHostUnused(ctx, dynamicClient, namespace); err != nil {
		return err
	}
	hasWireKubeState, err := nativewirekube.HasState(stateDirectory)
	if err != nil {
		return err
	}
	if hasWireKubeState {
		if err := nativewirekube.Revoke(ctx, nativewirekube.RevokeConfig{
			Dynamic: dynamicClient, StateDirectory: stateDirectory, WaitTimeout: timeout,
		}); err != nil {
			return fmt.Errorf("revoke host link: %w", err)
		}
	}
	if err := clientset.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		return fmt.Errorf("wait for host namespace deletion: %w", err)
	}
	if err := os.RemoveAll(stateDirectory); err != nil {
		return err
	}
	fmt.Printf("host/%s deleted\n", stateHostID)
	return nil
}

func loadClusterClients(ctx context.Context, kubeconfig, kubeContext, stateDirectory string, allowTOFU, resetTrust bool) (dynamic.Interface, kubernetes.Interface, error) {
	config, err := loadConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, nil, err
	}
	config, err = secureClusterConfig(ctx, config, stateDirectory, allowTOFU, resetTrust)
	if err != nil {
		return nil, nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return dynamicClient, clientset, nil
}

func namespaceOwnedByEnrollment(namespace *corev1.Namespace, identity enroll.EnrollmentIdentity) bool {
	return namespace != nil && namespace.Labels["app.kubernetes.io/managed-by"] == "idleloom" &&
		namespace.Labels["ai.idleloom.io/host-id"] == identity.HostID &&
		namespace.Annotations["ai.idleloom.io/enrollment-id"] == identity.Nonce
}

func ensureHostUnused(ctx context.Context, client dynamic.Interface, hostNamespace string) error {
	objects, err := client.Resource(nativekube.WorkloadsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("check workloads assigned to host: %w", err)
	}
	for index := range objects.Items {
		var workload nativev1alpha1.IdleloomWorkload
		if err := nativekube.FromUnstructured(&objects.Items[index], &workload); err != nil {
			return err
		}
		if workloadUsesHost(&workload, hostNamespace) {
			return fmt.Errorf("host is still referenced by workload/%s in namespace %s; delete the workload first", workload.Name, workload.Namespace)
		}
	}
	assignments, err := client.Resource(nativekube.AssignmentsGVR).Namespace(hostNamespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("check host assignments: %w", err)
	}
	if err == nil && len(assignments.Items) > 0 {
		return fmt.Errorf("host still has an assignment being cleaned up; wait and retry deletion")
	}
	return nil
}

func workloadUsesHost(workload *nativev1alpha1.IdleloomWorkload, hostNamespace string) bool {
	return workload != nil && workload.DeletionTimestamp == nil && workload.Status.SchedulingIntent != nil && workload.Status.SchedulingIntent.HostRef.Namespace == hostNamespace
}

const (
	resourceWorkloads = "workloads"
	resourceHosts     = "hosts"
	resourceWorkers   = "workers"
)

func parseResourceReference(args []string, allowBareWorkload bool) (string, string, error) {
	if len(args) == 0 || len(args) > 2 {
		return "", "", fmt.Errorf("a resource type is required")
	}
	if len(args) == 1 {
		parts := strings.Split(args[0], "/")
		if len(parts) == 1 {
			if allowBareWorkload {
				return resourceWorkloads, parts[0], nil
			}
			resourceName, err := canonicalResource(parts[0])
			return resourceName, "", err
		}
		if len(parts) != 2 || parts[1] == "" {
			return "", "", fmt.Errorf("resource references must use TYPE/NAME")
		}
		resourceName, err := canonicalResource(parts[0])
		return resourceName, parts[1], err
	}
	resourceName, err := canonicalResource(args[0])
	if err != nil {
		return "", "", err
	}
	if args[1] == "" || strings.Contains(args[1], "/") {
		return "", "", fmt.Errorf("resource name is invalid")
	}
	return resourceName, args[1], nil
}

func canonicalResource(value string) (string, error) {
	switch strings.ToLower(value) {
	case "workload", "workloads", "ilw", "idleloomworkload", "idleloomworkloads",
		"idleloomworkload.ai.idleloom.io", "idleloomworkloads.ai.idleloom.io":
		return resourceWorkloads, nil
	case "host", "hosts", "ilh", "idleloomhost", "idleloomhosts",
		"idleloomhost.ai.idleloom.io", "idleloomhosts.ai.idleloom.io":
		return resourceHosts, nil
	case "worker", "workers":
		return resourceWorkers, nil
	default:
		return "", fmt.Errorf("unknown resource %q; expected workloads, workers, or hosts", value)
	}
}

func validateOutputFormat(format string) error {
	switch format {
	case "table", "json", "yaml":
		return nil
	default:
		return fmt.Errorf("--output must be table, json, or yaml")
	}
}

func printStructured(value any, format string) error {
	var data []byte
	var err error
	switch format {
	case "json":
		data, err = json.MarshalIndent(value, "", "  ")
	case "yaml":
		data, err = yaml.Marshal(value)
	default:
		return fmt.Errorf("--output must be table, json, or yaml")
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}

func printWorkloadTable(workloads []nativev1alpha1.IdleloomWorkload, showNamespace bool) error {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if showNamespace {
		_, _ = fmt.Fprintln(writer, "NAMESPACE\tNAME\tTASK\tEXPERIMENT\tATTEMPT\tMODE\tPHASE\tMETRICS\tARTIFACTS\tDURATION")
	} else {
		_, _ = fmt.Fprintln(writer, "NAME\tTASK\tEXPERIMENT\tATTEMPT\tMODE\tPHASE\tMETRICS\tARTIFACTS\tDURATION")
	}
	for _, workload := range workloads {
		task, experiment, attempt := "-", "-", "-"
		if workload.Spec.Run != nil {
			task, experiment, attempt = workload.Spec.Run.Task, workload.Spec.Run.Experiment, fmt.Sprint(workload.Spec.Run.Attempt)
		}
		metrics, artifacts := 0, 0
		duration := "-"
		if workload.Status.Run != nil {
			metrics, artifacts = len(workload.Status.Run.Metrics), len(workload.Status.Run.Artifacts)
			if workload.Status.Run.StartedAt != nil {
				finished := time.Now()
				if workload.Status.Run.FinishedAt != nil {
					finished = workload.Status.Run.FinishedAt.Time
				}
				elapsed := finished.Sub(workload.Status.Run.StartedAt.Time)
				if elapsed < 0 {
					elapsed = 0
				}
				duration = elapsed.Round(time.Second).String()
			}
		}
		if showNamespace {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", workload.Namespace, workload.Name, task, experiment, attempt, workload.Spec.Mode, workload.Status.Phase, metrics, artifacts, duration)
		} else {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", workload.Name, task, experiment, attempt, workload.Spec.Mode, workload.Status.Phase, metrics, artifacts, duration)
		}
	}
	return writer.Flush()
}

func printHostTable(hosts []nativev1alpha1.IdleloomHost, showNamespace bool) error {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	now := time.Now()
	if showNamespace {
		_, _ = fmt.Fprintln(writer, "NAMESPACE\tNAME\tAGENT\tREADY\tCONNECTED\tSHELL")
	} else {
		_, _ = fmt.Fprintln(writer, "NAME\tAGENT\tREADY\tCONNECTED\tSHELL")
	}
	for _, host := range hosts {
		hostName := host.Labels["ai.idleloom.io/host-id"]
		if hostName == "" {
			hostName = host.Name
		}
		ready := liveHostConditionStatus(host, nativev1alpha1.HostConditionReady, now)
		connected := liveHostConditionStatus(host, nativev1alpha1.HostConditionConnected, now)
		if showNamespace {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", host.Namespace, hostName, host.Spec.AgentID, ready, connected, host.Spec.ShellAccess)
		} else {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", hostName, host.Spec.AgentID, ready, connected, host.Spec.ShellAccess)
		}
	}
	return writer.Flush()
}

func liveHostConditionStatus(host nativev1alpha1.IdleloomHost, conditionType string, now time.Time) string {
	status := conditionStatus(host.Status.Conditions, conditionType)
	if status != string(metav1.ConditionTrue) {
		return status
	}
	if host.Status.LastHeartbeatTime == nil {
		return string(metav1.ConditionUnknown)
	}
	heartbeat := host.Status.LastHeartbeatTime.Time
	if heartbeat.After(now.Add(nativev1alpha1.HeartbeatClockSkewAllowance)) {
		return string(metav1.ConditionUnknown)
	}
	if now.Sub(heartbeat) > nativev1alpha1.DefaultAgentHeartbeatTimeout+nativev1alpha1.HeartbeatClockSkewAllowance {
		return "Stale"
	}
	return status
}

func conditionStatus(conditions []metav1.Condition, conditionType string) string {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return string(condition.Status)
		}
	}
	return "Unknown"
}

func parseShellIsolation(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sandbox", "sandboxed":
		return nativev1alpha1.ShellIsolationSandbox, nil
	case "host":
		return nativev1alpha1.ShellIsolationHost, nil
	default:
		return "", fmt.Errorf("--isolation must be sandbox or host")
	}
}

func parseShellNetwork(value, isolation string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		if isolation == nativev1alpha1.ShellIsolationHost {
			return nativev1alpha1.ShellNetworkOutbound, nil
		}
		return nativev1alpha1.ShellNetworkNone, nil
	case "none":
		if isolation == nativev1alpha1.ShellIsolationHost {
			return "", fmt.Errorf("--network none is unavailable for host isolation; use outbound")
		}
		return nativev1alpha1.ShellNetworkNone, nil
	case "outbound":
		return nativev1alpha1.ShellNetworkOutbound, nil
	default:
		return "", fmt.Errorf("--network must be none or outbound")
	}
}

func clusterFlags(name string) (*flag.FlagSet, *string, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	kubeconfig := flags.String("kubeconfig", "", "kubeconfig path")
	kubeContext := flags.String("context", "", "kubeconfig context")
	return flags, kubeconfig, kubeContext
}

func clusterPFlags(name string) (*pflag.FlagSet, *string, *string) {
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	commandUsage := map[string]string{
		"join":   "idlectl join HOST [flags]",
		"run":    "idlectl run NAME --shell '<script>' [flags]",
		"get":    "idlectl get (hosts|workers|workloads) [NAME] [flags]",
		"logs":   "idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]",
		"delete": "idlectl delete ((host|worker|workload) NAME | (host|worker|workload)/NAME) [flags]",
	}[name]
	flags.Usage = func() {
		if commandUsage != "" {
			_, _ = fmt.Fprintf(flags.Output(), "Usage:\n  %s\n\nFlags:\n", commandUsage)
		}
		flags.PrintDefaults()
	}
	kubeconfig := flags.String("kubeconfig", "", "kubeconfig path")
	kubeContext := flags.String("context", "", "kubeconfig context")
	return flags, kubeconfig, kubeContext
}

func resolveNamespace(kubeconfig, kubeContext, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	namespace, _, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).Namespace()
	if err != nil {
		return "", err
	}
	if namespace == "" {
		return metav1.NamespaceDefault, nil
	}
	return namespace, nil
}

func loadConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("%w; pass --kubeconfig or set KUBECONFIG to select a cluster", err)
	}
	config.UserAgent = "idlectl"
	return config, nil
}

func secureClusterConfig(ctx context.Context, config *rest.Config, stateDirectory string, allowTOFU, resetTrust bool) (*rest.Config, error) {
	if !config.Insecure {
		return config, nil
	}
	if !allowTOFU {
		return nil, fmt.Errorf("source kubeconfig skips TLS verification; rerun with --allow-tofu to pin the observed API certificate")
	}
	return enroll.PinServerCertificate(ctx, config, stateDirectory, resetTrust)
}

func mustStateDirectory() string {
	directory, err := enroll.DefaultStateDirectory()
	if err != nil {
		return filepath.Join(os.TempDir(), "idleloom-state")
	}
	return directory
}

func waitForNativeAPI(ctx context.Context, client dynamic.Interface) error {
	deadline := time.NewTicker(500 * time.Millisecond)
	defer deadline.Stop()
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		_, err := client.Resource(nativekube.HostsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("native API did not become available: %w", err)
		case <-deadline.C:
		}
	}
}

func verifyIdentity(ctx context.Context, client kubernetes.Interface, expectedUser string) error {
	review, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("verify restricted Kubernetes identity: %w", err)
	}
	if review.Status.UserInfo.Username != expectedUser {
		return fmt.Errorf("kubeconfig authenticates as %q, expected %q", review.Status.UserInfo.Username, expectedUser)
	}
	return nil
}

func verifyRestrictedIdentity(ctx context.Context, client kubernetes.Interface, expectedUser, namespace string) error {
	if err := verifyIdentity(ctx, client, expectedUser); err != nil {
		return err
	}
	access, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "get", Resource: "secrets"}}}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("verify restricted Kubernetes permissions: %w", err)
	}
	if access.Status.Allowed {
		return fmt.Errorf("kubeconfig is over-privileged: it can read Secrets in namespace %s", namespace)
	}
	return nil
}
