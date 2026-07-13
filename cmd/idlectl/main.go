package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
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
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

const usageText = `idlectl manages Native Metal hosts and workloads.

Usage:
  idlectl join HOST [flags]
  idlectl run NAME --shell '<script>' [flags]
  idlectl get (hosts|workloads) [NAME] [flags]
  idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]
  idlectl delete ((host|workload) NAME | (host|workload)/NAME) [flags]
  idlectl version
`

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
	if internalErr != nil {
		if !errors.Is(internalErr, context.Canceled) && !errors.Is(internalErr, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "error:", internalErr)
			os.Exit(1)
		}
		return
	}
	if handled {
		return
	}
	if filepath.Base(os.Args[0]) != "idlectl" {
		fmt.Fprintf(os.Stderr, "unsupported executable name %q\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	handled, err := runPublicCommand(ctx, os.Args[1], os.Args[2:])
	if !handled {
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, pflag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runPublicCommand(ctx context.Context, command string, args []string) (bool, error) {
	switch command {
	case "join":
		return true, runJoin(ctx, args)
	case "run":
		return true, runWorkload(ctx, args)
	case "get":
		return true, runGet(ctx, args)
	case "logs":
		return true, runLogs(ctx, args)
	case "delete":
		return true, runDelete(ctx, args)
	case "version":
		fmt.Println(versionText())
		return true, nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return true, nil
	default:
		return false, nil
	}
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
		return fmt.Errorf("usage: idlectl join HOST [flags]")
	}
	hostID := enroll.NormalizeHostID(flags.Args()[0])
	if hostID == "" {
		return fmt.Errorf("HOST must contain a letter or digit")
	}
	if installed, err := serviceinstall.HasReceipt(*stateDirectory); err != nil {
		return err
	} else if installed {
		return fmt.Errorf("host is already joined locally; delete host/%s before joining again", hostID)
	}
	if err := requireServiceBinaries(*projectionEnabled, *link == nativewirekube.ConnectivityWireKube); err != nil {
		return err
	}
	var publicBinary []byte
	if *link == nativewirekube.ConnectivityWireKube {
		captured, captureErr := serviceinstall.CaptureCurrentBinary()
		if captureErr != nil {
			return fmt.Errorf("capture public native binary: %w", captureErr)
		}
		publicBinary = captured
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
	fmt.Fprintln(os.Stderr, "installing Native API and restricted identities")
	if err := install.Apply(ctx, dynamicClient, *forceConflicts); err != nil {
		return err
	}
	if *projectionEnabled {
		fmt.Fprintln(os.Stderr, "installing projection RBAC and admission policy")
		if err := install.ApplyProjection(ctx, dynamicClient, *forceConflicts); err != nil {
			return err
		}
	}
	if err := waitForNativeAPI(ctx, dynamicClient); err != nil {
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
	fmt.Fprintln(os.Stderr, "enrolled host; installing launchd services")
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
		ProjectionKubeconfig: projectionKubeconfig, PublicBinary: publicBinary,
		KubeletClientCommonNames: *kubeletClientCNs, KubeletClientOrganizations: *kubeletClientOrganizations,
	}); err != nil {
		rollbackErr := rollbackJoin(context.Background(), dynamicClient, clientset, *stateDirectory, result.Namespace)
		return errors.Join(fmt.Errorf("install native services: %w", err), rollbackErr)
	}
	fmt.Fprintln(os.Stderr, "waiting for host readiness")
	if err := waitForHostReady(ctx, dynamicClient, result.Namespace, result.Connectivity == nativewirekube.ConnectivityWireKube, 2*time.Minute); err != nil {
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
		fmt.Fprintf(config.Output, "using existing WireKube mesh %s (%s)\n", report.MeshName, report.MeshCIDR)
		for _, warning := range report.Warnings {
			fmt.Fprintln(config.Output, "warning:", warning)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("inspect WireKube installation: %w", err)
	}
	if config.Yes && !config.InstallDependencies {
		return fmt.Errorf("WireKube is not installed; rerun with --install-dependencies --yes, or use --link api-only")
	}
	if !config.Interactive && (!config.Yes || !config.InstallDependencies) {
		return fmt.Errorf("WireKube is not installed and input is non-interactive; rerun with --install-dependencies --yes, or use --link api-only")
	}

	fmt.Fprintf(config.Output, "WireKube is not installed; resolving compatible release %s\n", wirekubecli.CompatibleVersion)
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
	fmt.Fprintln(config.Output, "installing WireKube cluster dependencies")
	result, err := lifecycle.Install(ctx, plan)
	if err != nil {
		return fmt.Errorf("install WireKube: %w", err)
	}
	fmt.Fprintf(config.Output, "WireKube installation %s is ready; continuing host enrollment\n", result.InstallationID)
	return nil
}

func writeWireKubeJoinPlan(out io.Writer, config wireKubeJoinConfig, plan wirekubecli.Plan) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Idleloom connected-host plan")
	fmt.Fprintf(out, "  Host:          %s\n", config.HostID)
	fmt.Fprintf(out, "  Shell access:  %s\n", config.ShellAccess)
	fmt.Fprintf(out, "  Projection:    %t\n", config.Projection)
	fmt.Fprintf(out, "  Cluster:       %s\n", plan.Context)
	fmt.Fprintf(out, "  Kubernetes:    %s\n", plan.Detection.KubernetesVersion)
	fmt.Fprintf(out, "  CNI:           %s\n", plan.Detection.CNI)
	fmt.Fprintf(out, "  WireKube:      %s\n", plan.WireKubeVersion)
	fmt.Fprintf(out, "  Mesh CIDR:     %s\n", plan.MeshCIDR)
	fmt.Fprintf(out, "  Image:         %s\n", plan.Image)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Infrastructure impact")
	for _, impact := range plan.Impact {
		fmt.Fprintf(out, "  - %s\n", impact)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", warning)
	}
}

func confirmDefaultYes(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
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

func requireServiceBinaries(projection, linked bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	directory := filepath.Dir(executable)
	names := []string{"idleloom-controller", "idleloom-agent"}
	if projection {
		names = append(names, "idleloom-projection")
	}
	if linked {
		names = append(names, "idleloom-link")
	}
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("required service binary %s is missing; install the complete Idleloom Native bundle", path)
		}
	}
	return nil
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
	tunnel, err := nativewirekube.StartTunnel(ctx, state, func(format string, values ...any) {
		fmt.Fprintf(os.Stderr, "link: "+format+"\n", values...)
	})
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, tunnel.Close()) }()
	writeStatus := func() error {
		if err := tunnel.Validate(ctx); err != nil {
			return err
		}
		snapshot, snapshotErr := tunnel.Snapshot()
		status := nativewirekube.RuntimeStatus{
			Version: nativewirekube.RuntimeStatusVersion, InstanceID: lock.InstanceID, ProcessID: os.Getpid(),
			PeerUID: state.PeerUID, InterfaceName: tunnel.InterfaceName(),
			LastHandshakeTime: snapshot.LastHandshake, BytesReceived: snapshot.BytesReceived,
			BytesSent: snapshot.BytesSent, ObservedAt: time.Now().UTC(),
		}
		if snapshotErr != nil {
			status.Error = snapshotErr.Error()
		}
		return nativewirekube.WriteRuntimeStatus(runtimeDirectory, status)
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
		Logf: func(format string, values ...any) { fmt.Fprintf(os.Stderr, "controller: "+format+"\n", values...) },
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
	if err := verifyRestrictedIdentity(ctx, clientset, "system:serviceaccount:idleloom-system:idleloom-controller", "idleloom-system"); err != nil {
		return err
	}
	reconciler := &nativecontroller.Reconciler{Dynamic: dynamicClient, Coordination: clientset.CoordinationV1()}
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
		Logf: func(format string, values ...any) { fmt.Fprintf(os.Stderr, "controller: "+format+"\n", values...) },
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
			fmt.Fprintln(os.Stderr, "controller:", err)
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
			Logf: func(format string, values ...any) { fmt.Fprintf(os.Stderr, "projection: "+format+"\n", values...) },
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
		Logf: func(format string, values ...any) { fmt.Fprintf(os.Stderr, "projection: "+format+"\n", values...) },
	})
}

func runProjectionLoop(ctx context.Context, projector *nativeprojection.Controller, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := projector.ReconcileOnce(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "projection:", err)
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
		Logf: func(format string, values ...any) { fmt.Fprintf(os.Stderr, "agent: "+format+"\n", values...) },
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
		Dynamic: dynamicClient, Namespace: *namespace, AgentID: *agentID,
		Layout: devruntime.NewLayout(*root), StateDirectory: *stateDirectory,
		KubeconfigPath: *kubeconfig, ListenAddress: *listen,
		ConnectivityStatus: connectivityStatus,
		KubeletBridge:      kubeletBridge,
		Logf:               func(format string, values ...any) { fmt.Fprintf(os.Stderr, "agent: "+format+"\n", values...) },
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

func runWorkload(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("run")
	namespace := flags.StringP("namespace", "n", "", "workload namespace; defaults to the current context")
	script := flags.String("shell", "", "shell script to execute")
	isolation := flags.String("isolation", "sandbox", "shell isolation: sandbox or host")
	network := flags.String("network", "", "shell network access: sandbox defaults to none; host requires outbound")
	timeout := flags.Duration("timeout", time.Hour, "shell execution timeout, at most 24h")
	memory := flags.String("memory", "1Gi", "unified memory reservation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return fmt.Errorf("usage: idlectl run NAME --shell '<script>' [flags]")
	}
	name := flags.Args()[0]
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return fmt.Errorf("invalid workload name %q: %s", name, strings.Join(problems, "; "))
	}
	if strings.TrimSpace(*script) == "" {
		return fmt.Errorf("--shell is required")
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
	fmt.Fprintln(os.Stderr, "warning: shell commands are stored in Kubernetes API objects; do not include secrets")
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
			Labels: map[string]string{"app.kubernetes.io/managed-by": "idleloom"},
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode: nativev1alpha1.WorkloadModeShell,
			Shell: &nativev1alpha1.WorkloadShell{
				Script: *script, Isolation: resolvedIsolation, Network: resolvedNetwork,
				TimeoutSeconds: int32(*timeout / time.Second),
			},
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	positional := flags.Args()
	resourceName, name, err := parseResourceReference(positional, false)
	if err != nil {
		return fmt.Errorf("usage: idlectl get (hosts|workloads) [NAME] [flags]: %w", err)
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
		return fmt.Errorf("usage: idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]: %w", err)
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
	pods, err := clientset.CoreV1().Pods(resolvedNamespace).List(ctx, metav1.ListOptions{LabelSelector: nativeprojection.LabelWorkloadUID + "=" + string(workload.UID)})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("projected Pod for workload %s/%s is not ready", resolvedNamespace, workload.Name)
	}
	if len(pods.Items) != 1 {
		return fmt.Errorf("workload %s/%s has %d projected Pods", resolvedNamespace, workload.Name, len(pods.Items))
	}
	options := &corev1.PodLogOptions{Container: "native-metal", Follow: *follow, Timestamps: *timestamps}
	if *tail >= 0 {
		options.TailLines = tail
	}
	if *since > 0 {
		seconds := int64(*since / time.Second)
		options.SinceSeconds = &seconds
	}
	stream, err := clientset.CoreV1().Pods(resolvedNamespace).GetLogs(pods.Items[0].Name, options).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = io.Copy(os.Stdout, stream)
	return err
}

func runDelete(ctx context.Context, args []string) error {
	flags, kubeconfig, kubeContext := clusterPFlags("delete")
	namespace := flags.StringP("namespace", "n", "", "resource namespace; defaults to the current context")
	stateDirectory := flags.String("state-dir", mustStateDirectory(), "private enrolled host state")
	allowTOFU := flags.Bool("allow-tofu", false, "pin the currently observed API certificate when the source kubeconfig is insecure")
	resetTrust := flags.Bool("reset-trust", false, "replace the persisted API certificate identity after manual verification")
	waitForDeletion := flags.Bool("wait", true, "wait for the workload and its assignment to be removed")
	timeout := flags.Duration("timeout", time.Minute, "time to wait for deletion")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resourceName, name, err := parseResourceReference(flags.Args(), false)
	if err != nil || name == "" {
		return fmt.Errorf("usage: idlectl delete ((host|workload) NAME | (host|workload)/NAME) [flags]")
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
		return fmt.Errorf("read enrolled host identity: %w", err)
	}
	stateHostID := identity.HostID
	if enroll.NormalizeHostID(requestedHostID) != stateHostID {
		return fmt.Errorf("state belongs to host %q, not %q", stateHostID, requestedHostID)
	}
	config, err := loadConfig(kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	config, err = secureClusterConfig(ctx, config, stateDirectory, allowTOFU, resetTrust)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	namespace := "idleloom-host-" + stateHostID
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
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
	default:
		return "", fmt.Errorf("unknown resource %q; expected workloads or hosts", value)
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
		_, _ = fmt.Fprintln(writer, "NAMESPACE\tNAME\tMODE\tPHASE")
	} else {
		_, _ = fmt.Fprintln(writer, "NAME\tMODE\tPHASE")
	}
	for _, workload := range workloads {
		if showNamespace {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", workload.Namespace, workload.Name, workload.Spec.Mode, workload.Status.Phase)
		} else {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\n", workload.Name, workload.Spec.Mode, workload.Status.Phase)
		}
	}
	return writer.Flush()
}

func printHostTable(hosts []nativev1alpha1.IdleloomHost, showNamespace bool) error {
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
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
		if showNamespace {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", host.Namespace, hostName, host.Spec.AgentID, conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionReady), conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionConnected), host.Spec.ShellAccess)
		} else {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", hostName, host.Spec.AgentID, conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionReady), conditionStatus(host.Status.Conditions, nativev1alpha1.HostConditionConnected), host.Spec.ShellAccess)
		}
	}
	return writer.Flush()
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
		"get":    "idlectl get (hosts|workloads) [NAME] [flags]",
		"logs":   "idlectl logs (WORKLOAD | workload/WORKLOAD) [flags]",
		"delete": "idlectl delete ((host|workload) NAME | (host|workload)/NAME) [flags]",
	}[name]
	flags.Usage = func() {
		if commandUsage != "" {
			fmt.Fprintf(flags.Output(), "Usage:\n  %s\n\nFlags:\n", commandUsage)
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
		return nil, err
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

func verifyRestrictedIdentity(ctx context.Context, client kubernetes.Interface, expectedUser, namespace string) error {
	review, err := client.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("verify restricted Kubernetes identity: %w", err)
	}
	if review.Status.UserInfo.Username != expectedUser {
		return fmt.Errorf("kubeconfig authenticates as %q, expected %q", review.Status.UserInfo.Username, expectedUser)
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
