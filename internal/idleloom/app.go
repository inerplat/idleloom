package idleloom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	NetworkWireKube    = "wirekube"
	PhaseEnrolling     = "enrolling"
	PhaseRegistered    = "registered"
	PhaseReady         = "ready"
	PhaseLocalDeleting = "local-delete-pending"
	PhaseLocalGone     = "local-deleted"
)

type InitOptions struct {
	KubeconfigPath string
	Context        string
	NodeName       string
	CPUs           int
	MemoryMB       int
	DiskMB         int
	RuntimeDir     string
	Taint          string
	Network        string
	Timeout        time.Duration
	TokenTTL       time.Duration
	SkipWait       bool
	StatePath      string
	DryRun         bool
	// RegistryMirrors are raw HOST=URL specifications parsed and validated at
	// Init time. CredentialProvider* are host paths validated before any
	// side effect (including under --dry-run).
	RegistryMirrors          []string
	CredentialProviderBins   []string
	CredentialProviderConfig string
	CredentialProviderEnv    string
}

type App struct {
	Out                      io.Writer
	Err                      io.Writer
	Now                      func() time.Time
	Runtime                  WorkerRuntime
	DownloadKubelet          func(context.Context, string) (string, error)
	ApproveKubeletServingCSR func(context.Context, *Cluster, string, string, time.Time, bool, time.Duration) error
	StartMaintainer          func(context.Context, string, io.Writer) error
	StepIndex                int
}

func NewApp(out, errOut io.Writer) *App {
	runner := ExecRunner{}
	return &App{
		Out: out,
		Err: errOut,
		Now: time.Now,
		Runtime: KrunkitRuntime{
			Runner: runner,
			Out:    out,
			Err:    errOut,
		},
	}
}

func (a *App) Init(ctx context.Context, opts InitOptions) error {
	if err := validateInitOptions(opts); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("the krunkit worker backend currently requires macOS on Apple Silicon")
	}

	mirrors, mirrorWarnings, err := parseRegistryMirrors(opts.RegistryMirrors)
	if err != nil {
		return err
	}
	for _, warning := range mirrorWarnings {
		_, _ = fmt.Fprintf(a.Err, "warning: %s\n", warning)
	}
	if err := validateCredentialProviders(opts.CredentialProviderBins, opts.CredentialProviderConfig, opts.CredentialProviderEnv); err != nil {
		return err
	}

	a.step("Checking the Apple Silicon host")
	if err := a.Runtime.Preflight(ctx); err != nil {
		return err
	}

	a.step("Reading the Kubernetes cluster")
	cluster, err := LoadCluster(ctx, opts.KubeconfigPath, opts.Context)
	if err != nil {
		return err
	}
	if _, err := cluster.Client.CoreV1().Nodes().Get(ctx, opts.NodeName, metav1.GetOptions{}); err == nil {
		return fmt.Errorf("kubernetes node %q already exists in this cluster; pick a different name, or remove a previous Idleloom worker with \"idlectl delete worker %s\"", opts.NodeName, opts.NodeName)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("check existing Kubernetes node: %w", err)
	}
	_, _ = fmt.Fprintf(a.Out, "  Cluster: %s (%s)\n", cluster.Context, cluster.Version)
	_, _ = fmt.Fprintf(a.Out, "  API:     %s\n", cluster.Server)

	var wireKube WireKubeStatus
	if opts.Network == NetworkWireKube {
		a.step("Checking the WireKube node mesh")
		if opts.SkipWait {
			wireKube, err = checkWireKubeForRegistration(ctx, cluster.Client)
		} else {
			wireKube, err = CheckWireKube(ctx, cluster.Client)
		}
		if errors.Is(err, errNoReadyIngressPeers) {
			return fmt.Errorf("%w; a single-node mesh has no remote peers until this worker joins — register with \"idlectl create worker NAME --wait=false\", then run \"idlectl start worker\"", err)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Out, "  Agent:   %s/%s\n", wireKube.AgentNamespace, wireKube.AgentName)
		_, _ = fmt.Fprintf(a.Out, "  Peers:   %d ready\n", wireKube.ReadyPeers)
		if opts.SkipWait && wireKube.ReadyPeers == 0 {
			_, _ = fmt.Fprintln(a.Err, "warning: WireKube has no ready ingress peers; the registered worker will remain cordoned until \"idlectl start worker\" succeeds")
		}
	}
	a.step("Checking external worker compatibility")
	compatibility, err := CheckWorkerCompatibility(ctx, cluster)
	if err != nil {
		return err
	}
	for _, warning := range compatibility.Warnings {
		_, _ = fmt.Fprintf(a.Err, "warning: %s\n", warning)
	}

	if opts.DryRun {
		a.step("Planning the matching kubelet")
		_, _ = fmt.Fprintf(a.Out, "  Dry run: would download kubelet %s and create worker %s\n", cluster.KubeletVersion, opts.NodeName)
		return nil
	}
	a.step("Fetching the matching kubelet")
	kubeletPath, err := a.downloadKubelet(ctx, cluster.KubeletVersion)
	if err != nil {
		return err
	}
	statePath := opts.StatePath
	if statePath == "" {
		statePath, err = DefaultStatePath()
		if err != nil {
			return err
		}
	}
	stateLock, err := AcquireStateLock(ctx, statePath)
	if err != nil {
		return err
	}
	defer func() { _ = stateLock.Close() }()
	if err := EnsureStatePathAvailable(statePath); err != nil {
		return err
	}
	state := State{
		NodeName:        opts.NodeName,
		KubeconfigPath:  cluster.KubeconfigPath,
		Context:         cluster.Context,
		Network:         opts.Network,
		Taint:           opts.Taint,
		TaintConfigured: true,
		TokenTTLSeconds: durationSecondsCeil(opts.TokenTTL),
		Phase:           PhaseEnrolling,
		CreatedAt:       a.Now().UTC(),
		Runtime:         RuntimeState{NodeName: opts.NodeName},

		RegistryMirrors:          mirrors,
		CredentialProviderBins:   opts.CredentialProviderBins,
		CredentialProviderConfig: opts.CredentialProviderConfig,
		CredentialProviderEnv:    opts.CredentialProviderEnv,
	}
	reservationID, err := NewNetworkReservationID()
	if err != nil {
		return err
	}
	state.NetworkReservationID = reservationID
	if err := SaveState(statePath, state); err != nil {
		return errors.Join(err, removeStateFile(statePath))
	}

	a.step("Reserving an isolated worker network")
	runtimeNetwork, networkLease, networkLeaseUID, err := ReserveRuntimeNetwork(ctx, cluster.Client, opts.NodeName, state.NetworkReservationID)
	if err != nil {
		return fmt.Errorf("%w; reservation intent was saved to %s for recovery", err, statePath)
	}
	state.NetworkLease = networkLease
	state.NetworkLeaseUID = networkLeaseUID
	state.Runtime = RuntimeState{
		NodeName:   opts.NodeName,
		MACAddress: runtimeNetwork.MAC,
		Subnet:     runtimeNetwork.Subnet,
		GatewayIP:  runtimeNetwork.GatewayIP,
		GuestIP:    runtimeNetwork.GuestIP,
		HostIP:     runtimeNetwork.HostIP,
	}
	if err := SaveState(statePath, state); err != nil {
		releaseErr := ReleaseRuntimeNetwork(context.Background(), cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID)
		if releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release network reservation: %w; recovery state remains at %s", releaseErr, statePath))
		}
		return errors.Join(err, removeStateFile(statePath))
	}
	_, _ = fmt.Fprintf(a.Out, "  Guest:   %s (%s)\n", runtimeNetwork.GuestIP, runtimeNetwork.Subnet)

	plannedRuntime, err := a.Runtime.Plan(ctx, RuntimeConfig{
		NodeName: opts.NodeName, CPUs: opts.CPUs, MemoryMB: opts.MemoryMB,
		DiskMB: opts.DiskMB, RuntimeDir: opts.RuntimeDir, Network: runtimeNetwork,
	})
	if err != nil {
		releaseErr := ReleaseRuntimeNetwork(context.Background(), cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID)
		if releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release network reservation: %w; recovery state remains at %s", releaseErr, statePath))
		}
		return errors.Join(err, removeStateFile(statePath))
	}
	state.Runtime = plannedRuntime
	if err := SaveState(statePath, state); err != nil {
		releaseErr := ReleaseRuntimeNetwork(context.Background(), cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID)
		if releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release network reservation: %w; recovery state remains at %s", releaseErr, statePath))
		}
		return errors.Join(err, removeStateFile(statePath))
	}

	a.step("Creating the krunkit worker VM")
	if err := a.Runtime.Create(ctx, &state.Runtime); err != nil {
		saveErr := SaveState(statePath, state)
		return errors.Join(fmt.Errorf("%w; recovery state was saved to %s", err, statePath), saveErr)
	}
	if err := SaveState(statePath, state); err != nil {
		return fmt.Errorf("save created runtime state: %w; recovery state remains at %s", err, statePath)
	}

	a.step("Creating a short-lived TLS bootstrap identity")
	token, err := CreateBootstrapToken(ctx, cluster.Client, opts.TokenTTL)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := token.Delete(cleanupCtx); err != nil {
			_, _ = fmt.Fprintf(a.Err, "warning: %v\n", err)
		}
	}()

	bundlePath, cleanupBundle, err := CreateWorkerBundle(BundleConfig{
		NodeName:      opts.NodeName,
		Taint:         opts.Taint,
		Server:        cluster.Server,
		TLSServerName: cluster.TLSServerName,
		CAData:        cluster.CAData,
		Token:         token.Value,
		ClusterDNS:    cluster.ClusterDNS,
		ClusterDomain: cluster.ClusterDomain,
		KubeletPath:   kubeletPath,

		RegistryMirrors:          mirrors,
		CredentialProviderBins:   opts.CredentialProviderBins,
		CredentialProviderConfig: opts.CredentialProviderConfig,
		CredentialProviderEnv:    opts.CredentialProviderEnv,
	})
	if err != nil {
		return err
	}
	defer cleanupBundle()
	if err := a.Runtime.InstallBundle(ctx, state.Runtime, bundlePath); err != nil {
		return err
	}

	a.step("Waiting for kubelet TLS bootstrap")
	if err := waitForNode(ctx, cluster, opts.NodeName, opts.Timeout); err != nil {
		return err
	}
	if err := labelNode(ctx, cluster, opts.NodeName, opts.Network); err != nil {
		return err
	}
	a.step("Approving the kubelet serving certificate")
	if err := a.approveKubeletServingCSR(ctx, cluster, opts.NodeName, state.Runtime.GuestIP, state.CreatedAt, true, opts.Timeout); err != nil {
		return err
	}
	if opts.SkipWait {
		return a.registerWorkerWithoutWaiting(ctx, statePath, &state, cluster, token)
	}

	if opts.Network == NetworkWireKube {
		a.step("Waiting for the WireKube tunnel")
		_, _ = fmt.Fprintln(a.Out, "  The first handshake may take a few minutes while the CNI becomes ready.")
		if err := waitForWireKubeAgent(ctx, cluster, opts.NodeName, wireKube, opts.Timeout); err != nil {
			return err
		}
		if err := waitForWireKube(ctx, cluster, opts.NodeName, opts.Timeout); err != nil {
			return err
		}
	}

	a.step("Waiting for the worker to become Ready")
	if err := waitForNodeReady(ctx, cluster, opts.NodeName, opts.Timeout); err != nil {
		return err
	}
	if err := a.removeBootstrapIdentity(ctx, token, state.Runtime); err != nil {
		return err
	}
	state.Phase = PhaseReady
	if err := SaveState(statePath, state); err != nil {
		return err
	}
	if err := a.startMaintainer(ctx, statePath); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is Ready.\n", opts.NodeName)
	return nil
}

func (a *App) Start(ctx context.Context, statePath string, timeout time.Duration) error {
	resolvedPath, err := resolveStatePath(statePath)
	if err != nil {
		return err
	}
	stateLock, err := AcquireStateLock(ctx, resolvedPath)
	if err != nil {
		return err
	}
	defer func() { _ = stateLock.Close() }()
	state, err := LoadState(resolvedPath)
	if err != nil {
		return err
	}
	cluster, err := LoadCluster(ctx, state.KubeconfigPath, state.Context)
	if err != nil {
		return err
	}
	if err := ValidateRuntimeNetworkReservation(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID, state.Runtime); err != nil {
		return err
	}
	if state.Phase == PhaseEnrolling {
		return a.resumeEnrollment(ctx, resolvedPath, &state, cluster, timeout)
	}
	if state.Phase == PhaseRegistered {
		return a.completeRegisteredEnrollment(ctx, resolvedPath, &state, cluster, timeout)
	}
	if state.Phase != PhaseReady {
		if state.Phase == PhaseLocalDeleting {
			return fmt.Errorf("this worker is partially deleted; finish removal with \"idlectl delete worker %s --local-only\", then recreate it with \"idlectl create worker\"", state.NodeName)
		}
		return fmt.Errorf("worker state phase %q cannot be started; run \"idlectl status\" to inspect", state.Phase)
	}
	previousNode, err := cluster.Client.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node %s before start: %w", state.NodeName, err)
	}
	previousHeartbeat := nodeHeartbeat(previousNode)
	previousSSHPort := state.Runtime.SSHPort
	runtimeStatus, err := a.Runtime.Status(ctx, &state.Runtime)
	if err != nil {
		return err
	}
	if state.Runtime.SSHPort != previousSSHPort {
		if err := SaveState(resolvedPath, state); err != nil {
			return err
		}
	}
	wasRunning := runtimeStatus.VM == "running" && runtimeStatus.Network == "running"
	var wireKube WireKubeStatus
	if state.Network == NetworkWireKube {
		wireKube, err = CheckWireKube(ctx, cluster.Client)
		if err != nil {
			return err
		}
	}
	a.step("Starting the krunkit worker VM")
	startNotBefore := state.CreatedAt
	if err := a.Runtime.Start(ctx, &state.Runtime); err != nil {
		return err
	}
	if err := SaveState(resolvedPath, state); err != nil {
		_ = a.Runtime.Stop(context.Background(), state.Runtime)
		return err
	}
	if err := a.Runtime.WaitReady(ctx, state.Runtime, 5*time.Minute); err != nil {
		return err
	}
	a.step("Waiting for kubelet to reconnect")
	if wasRunning {
		if err := waitForNodeReady(ctx, cluster, state.NodeName, timeout); err != nil {
			return err
		}
	} else {
		if err := waitForNodeReadyAfter(ctx, cluster, state.NodeName, previousHeartbeat, timeout); err != nil {
			return err
		}
	}
	a.step("Checking the kubelet serving certificate")
	if err := a.approveKubeletServingCSR(ctx, cluster, state.NodeName, state.Runtime.GuestIP, startNotBefore, false, timeout); err != nil {
		return err
	}
	if state.Network == NetworkWireKube {
		a.step("Waiting for the WireKube tunnel")
		if err := waitForWireKubeAgent(ctx, cluster, state.NodeName, wireKube, timeout); err != nil {
			return err
		}
		if err := waitForWireKube(ctx, cluster, state.NodeName, timeout); err != nil {
			return err
		}
	}
	if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, state.NodeName, types.MergePatchType, []byte(`{"spec":{"unschedulable":false}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("uncordon node %s: %w", state.NodeName, err)
	}
	if err := a.startMaintainer(ctx, resolvedPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is Ready.\n", state.NodeName)
	return nil
}

func (a *App) registerWorkerWithoutWaiting(ctx context.Context, statePath string, state *State, cluster *Cluster, token *BootstrapToken) error {
	if state == nil || state.Phase != PhaseEnrolling {
		return fmt.Errorf("an enrolling worker state is required")
	}
	a.step("Deferring worker readiness")
	if err := cordonNode(ctx, cluster, state.NodeName); err != nil {
		return err
	}
	if err := a.removeBootstrapIdentity(ctx, token, state.Runtime); err != nil {
		return err
	}
	state.Phase = PhaseRegistered
	if err := SaveState(statePath, *state); err != nil {
		return err
	}
	if err := a.startMaintainer(ctx, statePath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is registered; readiness is pending.\n", state.NodeName)
	_, _ = fmt.Fprintln(a.Out, "The Kubernetes Node remains cordoned. Run \"idlectl status\", then \"idlectl start worker\" to complete readiness.")
	return nil
}

func (a *App) completeRegisteredEnrollment(ctx context.Context, statePath string, state *State, cluster *Cluster, timeout time.Duration) error {
	if state == nil || state.Phase != PhaseRegistered {
		return fmt.Errorf("a registered worker state is required")
	}
	previousNode, err := cluster.Client.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get registered node %s before start: %w", state.NodeName, err)
	}
	previousHeartbeat := nodeHeartbeat(previousNode)
	previousSSHPort := state.Runtime.SSHPort
	runtimeStatus, err := a.Runtime.Status(ctx, &state.Runtime)
	if err != nil {
		return err
	}
	if state.Runtime.SSHPort != previousSSHPort {
		if err := SaveState(statePath, *state); err != nil {
			return err
		}
	}
	wasRunning := runtimeStatus.VM == "running" && runtimeStatus.Network == "running"

	a.step("Completing registered worker enrollment")
	if err := a.Runtime.Start(ctx, &state.Runtime); err != nil {
		return err
	}
	if err := SaveState(statePath, *state); err != nil {
		_ = a.Runtime.Stop(context.Background(), state.Runtime)
		return err
	}
	if err := a.Runtime.WaitReady(ctx, state.Runtime, 5*time.Minute); err != nil {
		return err
	}
	a.step("Waiting for kubelet to reconnect")
	if err := waitForNode(ctx, cluster, state.NodeName, timeout); err != nil {
		return err
	}
	a.step("Checking the kubelet serving certificate")
	if err := a.approveKubeletServingCSR(ctx, cluster, state.NodeName, state.Runtime.GuestIP, state.CreatedAt, false, timeout); err != nil {
		return err
	}
	if state.Network == NetworkWireKube {
		wireKube, err := CheckWireKube(ctx, cluster.Client)
		if err != nil {
			return err
		}
		a.step("Waiting for the WireKube tunnel")
		if err := waitForWireKubeAgent(ctx, cluster, state.NodeName, wireKube, timeout); err != nil {
			return err
		}
		if err := waitForWireKube(ctx, cluster, state.NodeName, timeout); err != nil {
			return err
		}
	}
	a.step("Waiting for the worker to become Ready")
	if wasRunning {
		err = waitForNodeReady(ctx, cluster, state.NodeName, timeout)
	} else {
		err = waitForNodeReadyAfter(ctx, cluster, state.NodeName, previousHeartbeat, timeout)
	}
	if err != nil {
		return err
	}
	if err := uncordonNode(ctx, cluster, state.NodeName); err != nil {
		return err
	}
	state.Phase = PhaseReady
	if err := SaveState(statePath, *state); err != nil {
		return err
	}
	if err := a.startMaintainer(ctx, statePath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is Ready.\n", state.NodeName)
	return nil
}

func (a *App) removeBootstrapIdentity(ctx context.Context, token *BootstrapToken, runtime RuntimeState) error {
	if token != nil {
		if err := token.Delete(ctx); err != nil {
			return err
		}
		token.client = nil
	}
	return a.Runtime.RemoveBootstrapIdentity(ctx, runtime)
}

func (a *App) startMaintainer(ctx context.Context, statePath string) error {
	if a.StartMaintainer != nil {
		return a.StartMaintainer(ctx, statePath, a.Err)
	}
	return startMaintainer(ctx, statePath, a.Err)
}

func (a *App) resumeEnrollment(ctx context.Context, statePath string, state *State, cluster *Cluster, timeout time.Duration) error {
	if state == nil || state.Phase != PhaseEnrolling {
		return fmt.Errorf("an enrolling worker state is required")
	}
	if state.Runtime.Planned {
		return fmt.Errorf("worker enrollment stopped before the VM was created; delete the local state with \"idlectl delete worker %s --local-only --force --state %s\", then run \"idlectl create worker\" again", state.NodeName, statePath)
	}
	_, nodeErr := cluster.Client.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{})
	if nodeErr != nil && !apierrors.IsNotFound(nodeErr) {
		return fmt.Errorf("inspect interrupted worker enrollment: %w", nodeErr)
	}
	nodeMissing := apierrors.IsNotFound(nodeErr)
	if nodeMissing && !state.TaintConfigured {
		return fmt.Errorf("interrupted enrollment state predates resumable taint metadata and Kubernetes Node %q is absent; delete the local state with \"idlectl delete worker %s --local-only --force --state %s\", then run \"idlectl create worker\" again", state.NodeName, state.NodeName, statePath)
	}
	if state.TaintConfigured {
		if err := validateTaint(state.Taint); err != nil {
			return fmt.Errorf("interrupted enrollment state has an invalid taint: %w", err)
		}
		if state.TokenTTLSeconds < 0 || state.TokenTTLSeconds > math.MaxInt64/int64(time.Second) {
			return fmt.Errorf("interrupted enrollment state has an invalid bootstrap token lifetime")
		}
	}
	a.step("Resuming the interrupted worker enrollment")
	previousSSHPort := state.Runtime.SSHPort
	if err := a.Runtime.Start(ctx, &state.Runtime); err != nil {
		return err
	}
	if state.Runtime.SSHPort != previousSSHPort {
		if err := SaveState(statePath, *state); err != nil {
			_ = a.Runtime.Stop(context.Background(), state.Runtime)
			return err
		}
	}
	if err := a.Runtime.WaitReady(ctx, state.Runtime, 5*time.Minute); err != nil {
		return err
	}
	servingNotBefore := state.CreatedAt
	var token *BootstrapToken
	if state.TaintConfigured {
		a.step("Refreshing the interrupted TLS bootstrap identity")
		if nodeMissing {
			servingNotBefore = a.Now().UTC()
		}
		kubeletPath, err := a.downloadKubelet(ctx, cluster.KubeletVersion)
		if err != nil {
			return err
		}
		tokenTTL := time.Duration(state.TokenTTLSeconds) * time.Second
		if tokenTTL <= 0 {
			tokenTTL = 30 * time.Minute
		}
		token, err = CreateBootstrapToken(ctx, cluster.Client, tokenTTL)
		if err != nil {
			return err
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := token.Delete(cleanupCtx); err != nil {
				_, _ = fmt.Fprintf(a.Err, "warning: %v\n", err)
			}
		}()
		if err := validateCredentialProviders(state.CredentialProviderBins, state.CredentialProviderConfig, state.CredentialProviderEnv); err != nil {
			return fmt.Errorf("cannot rebuild the interrupted worker bundle: %w", err)
		}
		bundlePath, cleanupBundle, err := CreateWorkerBundle(BundleConfig{
			NodeName: state.NodeName, Taint: state.Taint, Server: cluster.Server,
			TLSServerName: cluster.TLSServerName, CAData: cluster.CAData, Token: token.Value,
			ClusterDNS: cluster.ClusterDNS, ClusterDomain: cluster.ClusterDomain, KubeletPath: kubeletPath,
			RegistryMirrors:          state.RegistryMirrors,
			CredentialProviderBins:   state.CredentialProviderBins,
			CredentialProviderConfig: state.CredentialProviderConfig,
			CredentialProviderEnv:    state.CredentialProviderEnv,
		})
		if err != nil {
			return err
		}
		defer cleanupBundle()
		if err := a.Runtime.InstallBundle(ctx, state.Runtime, bundlePath); err != nil {
			return err
		}
	}
	a.step("Waiting for kubelet TLS bootstrap")
	if err := waitForNode(ctx, cluster, state.NodeName, timeout); err != nil {
		return err
	}
	if err := labelNode(ctx, cluster, state.NodeName, state.Network); err != nil {
		return err
	}
	a.step("Approving the kubelet serving certificate")
	if err := a.approveKubeletServingCSR(ctx, cluster, state.NodeName, state.Runtime.GuestIP, servingNotBefore, true, timeout); err != nil {
		return err
	}
	if state.Network == NetworkWireKube {
		wireKube, err := CheckWireKube(ctx, cluster.Client)
		if err != nil {
			return err
		}
		a.step("Waiting for the WireKube tunnel")
		if err := waitForWireKubeAgent(ctx, cluster, state.NodeName, wireKube, timeout); err != nil {
			return err
		}
		if err := waitForWireKube(ctx, cluster, state.NodeName, timeout); err != nil {
			return err
		}
	}
	a.step("Waiting for the worker to become Ready")
	if err := waitForNodeReady(ctx, cluster, state.NodeName, timeout); err != nil {
		return err
	}
	if err := a.removeBootstrapIdentity(ctx, token, state.Runtime); err != nil {
		return err
	}
	state.Phase = PhaseReady
	if err := SaveState(statePath, *state); err != nil {
		return err
	}
	if err := a.startMaintainer(ctx, statePath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s enrollment resumed and is Ready.\n", state.NodeName)
	return nil
}

func (a *App) downloadKubelet(ctx context.Context, version string) (string, error) {
	if a.DownloadKubelet != nil {
		return a.DownloadKubelet(ctx, version)
	}
	return DownloadKubelet(ctx, version)
}

func (a *App) approveKubeletServingCSR(ctx context.Context, cluster *Cluster, nodeName, guestIP string, notBefore time.Time, wait bool, timeout time.Duration) error {
	if a.ApproveKubeletServingCSR != nil {
		return a.ApproveKubeletServingCSR(ctx, cluster, nodeName, guestIP, notBefore, wait, timeout)
	}
	return ApproveKubeletServingCSR(ctx, cluster.Client, nodeName, guestIP, notBefore, wait, timeout)
}

func durationSecondsCeil(duration time.Duration) int64 {
	seconds := int64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	return seconds
}

func (a *App) Stop(ctx context.Context, statePath string, localOnly bool) error {
	resolvedPath, err := resolveStatePath(statePath)
	if err != nil {
		return err
	}
	stateLock, err := AcquireStateLock(ctx, resolvedPath)
	if err != nil {
		return err
	}
	defer func() { _ = stateLock.Close() }()
	state, err := LoadState(resolvedPath)
	if err != nil {
		return err
	}
	if localOnly {
		a.step("Stopping the local krunkit worker VM")
		if err := a.Runtime.Stop(ctx, state.Runtime); err != nil {
			return err
		}
		if err := stopMaintainer(resolvedPath); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is stopped locally; the Kubernetes Node was not cordoned.\n", state.NodeName)
		return nil
	}
	if err := a.Runtime.Validate(ctx, state.Runtime); err != nil {
		return err
	}
	cluster, err := LoadCluster(ctx, state.KubeconfigPath, state.Context)
	if err != nil {
		return err
	}
	if err := ValidateRuntimeNetworkReservation(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID, state.Runtime); err != nil {
		return err
	}
	node, err := cluster.Client.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get node %s before stop: %w", state.NodeName, err)
	}
	wasSchedulable := node != nil && !node.Spec.Unschedulable
	if wasSchedulable {
		if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, state.NodeName, types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("cordon node %s: %w", state.NodeName, err)
		}
	}
	busy, err := activeWorkloadPods(ctx, cluster, state.NodeName)
	if err != nil {
		if wasSchedulable {
			_ = uncordonNode(context.Background(), cluster, state.NodeName)
		}
		return err
	}
	if len(busy) > 0 {
		if wasSchedulable {
			if rollbackErr := uncordonNode(ctx, cluster, state.NodeName); rollbackErr != nil {
				return fmt.Errorf("worker has active workload pods: %s; additionally failed to restore scheduling: %w", strings.Join(busy, ", "), rollbackErr)
			}
		}
		return fmt.Errorf("worker still has active workload pods: %s; drain or remove them before stopping", strings.Join(busy, ", "))
	}
	a.step("Stopping the krunkit worker VM")
	if err := a.Runtime.Stop(ctx, state.Runtime); err != nil {
		return err
	}
	if err := stopMaintainer(resolvedPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is stopped and cordoned.\n", state.NodeName)
	return nil
}

func (a *App) Delete(ctx context.Context, statePath string, force, localOnly bool) error {
	resolvedPath, err := resolveStatePath(statePath)
	if err != nil {
		return err
	}
	stateLock, err := AcquireStateLock(ctx, resolvedPath)
	if err != nil {
		return err
	}
	defer func() { _ = stateLock.Close() }()
	state, err := LoadState(resolvedPath)
	if err != nil {
		return err
	}
	if err := a.Runtime.Validate(ctx, state.Runtime); err != nil {
		return err
	}
	if localOnly {
		a.step("Deleting the local krunkit worker VM")
		state.Phase = PhaseLocalDeleting
		if err := SaveState(resolvedPath, state); err != nil {
			return err
		}
		if err := stopMaintainer(resolvedPath); err != nil {
			return err
		}
		if err := a.Runtime.Delete(ctx, state.Runtime); err != nil {
			return err
		}
		state.Phase = PhaseLocalGone
		if err := SaveState(resolvedPath, state); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s was deleted locally; run delete again without --local-only when Kubernetes recovers.\n", state.NodeName)
		return nil
	}
	cluster, err := LoadCluster(ctx, state.KubeconfigPath, state.Context)
	if err != nil {
		return err
	}
	if state.NetworkLease == "" && state.NetworkReservationID != "" {
		network, leaseName, leaseUID, found, err := FindRuntimeNetworkReservation(ctx, cluster.Client, state.NodeName, state.NetworkReservationID)
		if err != nil {
			return err
		}
		if found {
			state.NetworkLease = leaseName
			state.NetworkLeaseUID = leaseUID
			state.Runtime.MACAddress = network.MAC
			state.Runtime.Subnet = network.Subnet
			state.Runtime.GatewayIP = network.GatewayIP
			state.Runtime.GuestIP = network.GuestIP
			state.Runtime.HostIP = network.HostIP
			if err := SaveState(resolvedPath, state); err != nil {
				return err
			}
		}
	}
	if state.NetworkLease != "" {
		if err := ValidateRuntimeNetworkReservation(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID, state.Runtime); err != nil && (state.Phase != PhaseLocalGone || !errors.Is(err, ErrRuntimeNetworkReservationNotFound)) {
			return err
		}
	}
	nodeExists := false
	wasSchedulable := false
	if node, err := cluster.Client.CoreV1().Nodes().Get(ctx, state.NodeName, metav1.GetOptions{}); err == nil {
		nodeExists = true
		wasSchedulable = !node.Spec.Unschedulable
		if wasSchedulable {
			if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, state.NodeName, types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("cordon node %s before deletion: %w", state.NodeName, err)
			}
		}
		busy, listErr := activeWorkloadPods(ctx, cluster, state.NodeName)
		if listErr != nil {
			if wasSchedulable {
				_ = uncordonNode(context.Background(), cluster, state.NodeName)
			}
			return listErr
		}
		if len(busy) > 0 && !force {
			if wasSchedulable {
				if rollbackErr := uncordonNode(ctx, cluster, state.NodeName); rollbackErr != nil {
					return fmt.Errorf("worker has active workload pods: %s; additionally failed to restore scheduling: %w", strings.Join(busy, ", "), rollbackErr)
				}
			}
			return fmt.Errorf("worker still has active workload pods: %s; retry with --force to delete it", strings.Join(busy, ", "))
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Kubernetes node %s: %w", state.NodeName, err)
	}
	if state.Phase != PhaseLocalGone {
		previousPhase := state.Phase
		state.Phase = PhaseLocalDeleting
		if err := SaveState(resolvedPath, state); err != nil {
			if wasSchedulable {
				_ = uncordonNode(context.Background(), cluster, state.NodeName)
			}
			return err
		}
		if err := stopMaintainer(resolvedPath); err != nil {
			state.Phase = previousPhase
			stateErr := SaveState(resolvedPath, state)
			var schedulingErr error
			if wasSchedulable {
				schedulingErr = uncordonNode(context.Background(), cluster, state.NodeName)
			}
			return errors.Join(err, stateErr, schedulingErr)
		}
		a.step("Deleting the krunkit worker VM")
		if err := a.Runtime.Delete(ctx, state.Runtime); err != nil {
			return err
		}
		state.Phase = PhaseLocalGone
		if err := SaveState(resolvedPath, state); err != nil {
			return err
		}
	} else if err := stopMaintainer(resolvedPath); err != nil {
		return err
	}
	if nodeExists {
		if err := cluster.Client.CoreV1().Nodes().Delete(ctx, state.NodeName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Kubernetes node %s: %w", state.NodeName, err)
		}
	}
	if err := ReleaseRuntimeNetwork(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID); err != nil {
		return err
	}
	if err := cleanupMaintainerFiles(resolvedPath); err != nil {
		return err
	}
	if err := os.Remove(resolvedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Idleloom state %s: %w", resolvedPath, err)
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s was deleted.\n", state.NodeName)
	return nil
}

func uncordonNode(ctx context.Context, cluster *Cluster, nodeName string) error {
	if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(`{"spec":{"unschedulable":false}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("uncordon node %s: %w", nodeName, err)
	}
	return nil
}

func cordonNode(ctx context.Context, cluster *Cluster, nodeName string) error {
	if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("cordon node %s: %w", nodeName, err)
	}
	return nil
}

func removeStateFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Idleloom state %s: %w", path, err)
	}
	return nil
}

func validateInitOptions(opts InitOptions) error {
	if problems := validation.IsDNS1123Subdomain(opts.NodeName); len(problems) > 0 {
		return fmt.Errorf("invalid node name %q: %v", opts.NodeName, problems)
	}
	if opts.CPUs < 2 {
		return fmt.Errorf("at least 2 CPUs are required")
	}
	if opts.MemoryMB < 4096 {
		return fmt.Errorf("at least 4096 MiB of memory is required by the krunkit GPU VM")
	}
	if opts.DiskMB < 6144 {
		return fmt.Errorf("at least 6144 MiB of disk is required")
	}
	if err := validateTaint(opts.Taint); err != nil {
		return err
	}
	if opts.Network != NetworkWireKube {
		return fmt.Errorf("network must be %q; direct routing is not supported by the gvproxy backend", NetworkWireKube)
	}
	if opts.Timeout <= 0 || opts.TokenTTL <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	return nil
}

func validateTaint(taint string) error {
	if taint == "" {
		return nil
	}
	colon := strings.LastIndex(taint, ":")
	if colon <= 0 || colon == len(taint)-1 {
		return fmt.Errorf("taint must use key=value:effect syntax")
	}
	effect := taint[colon+1:]
	if effect != "NoSchedule" && effect != "PreferNoSchedule" && effect != "NoExecute" {
		return fmt.Errorf("unsupported taint effect %q", effect)
	}
	keyValue := taint[:colon]
	equals := strings.Index(keyValue, "=")
	if equals <= 0 {
		return fmt.Errorf("taint must use key=value:effect syntax")
	}
	key, value := keyValue[:equals], keyValue[equals+1:]
	if problems := validation.IsQualifiedName(key); len(problems) > 0 {
		return fmt.Errorf("invalid taint key %q: %v", key, problems)
	}
	if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
		return fmt.Errorf("invalid taint value %q: %v", value, problems)
	}
	return nil
}

func resolveStatePath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultStatePath()
}

func activeWorkloadPods(ctx context.Context, cluster *Cluster, nodeName string) ([]string, error) {
	pods, err := cluster.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
	if err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", nodeName, err)
	}
	var active []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		daemonSet := false
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" {
				daemonSet = true
				break
			}
		}
		if daemonSet {
			continue
		}
		active = append(active, pod.Namespace+"/"+pod.Name)
	}
	return active, nil
}

func labelNode(ctx context.Context, cluster *Cluster, nodeName, network string) error {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "idleloom",
		"idleloom-worker":              "true",
		"idleloom-runtime":             "krunkit",
		"idleloom-accelerator":         "apple-vulkan",
	}
	if network == NetworkWireKube {
		labels["wirekube.io/vpn-enabled"] = "true"
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("encode node labels: %w", err)
	}
	if _, err := cluster.Client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label Kubernetes node %s: %w", nodeName, err)
	}
	return nil
}

func waitForNode(ctx context.Context, cluster *Cluster, nodeName string, timeout time.Duration) error {
	return poll(ctx, timeout, func() (bool, error) {
		_, err := cluster.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}, "node registration")
}

func waitForNodeReady(ctx context.Context, cluster *Cluster, nodeName string, timeout time.Duration) error {
	return poll(ctx, timeout, func() (bool, error) {
		node, err := cluster.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return nodeReady(node), nil
	}, "node readiness")
}

func waitForNodeReadyAfter(ctx context.Context, cluster *Cluster, nodeName string, previousHeartbeat time.Time, timeout time.Duration) error {
	return poll(ctx, timeout, func() (bool, error) {
		node, err := cluster.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return nodeReady(node) && nodeHeartbeat(node).After(previousHeartbeat), nil
	}, "a fresh kubelet heartbeat")
}

func nodeHeartbeat(node *corev1.Node) time.Time {
	if node == nil {
		return time.Time{}
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.LastHeartbeatTime.Time
		}
	}
	return time.Time{}
}

func waitForWireKubeAgent(ctx context.Context, cluster *Cluster, nodeName string, status WireKubeStatus, timeout time.Duration) error {
	return poll(ctx, timeout, func() (bool, error) {
		pods, err := cluster.Client.CoreV1().Pods(status.AgentNamespace).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + nodeName})
		if err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if !strings.HasPrefix(pod.Name, status.AgentName+"-") {
				continue
			}
			for _, container := range pod.Status.ContainerStatuses {
				if container.Ready {
					return true, nil
				}
			}
		}
		return false, nil
	}, "WireKube agent readiness")
}

func waitForWireKube(ctx context.Context, cluster *Cluster, nodeName string, timeout time.Duration) error {
	return poll(ctx, timeout, func() (bool, error) {
		connected, err := WireKubePeerConnected(ctx, cluster.Client, nodeName)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return connected, err
	}, "WireKube peer connection")
}

func poll(ctx context.Context, timeout time.Duration, check func() (bool, error), description string) error {
	return pollWithInterval(ctx, timeout, 2*time.Second, check, description)
}

func pollWithInterval(ctx context.Context, timeout, interval time.Duration, check func() (bool, error), description string) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	unauthorizedRetries := 0
	for {
		ready, err := check()
		if err != nil {
			// Exec-based kubeconfigs can rotate credentials while a long enrollment
			// is polling. Give the transport time to refresh an expired credential.
			if apierrors.IsUnauthorized(err) && unauthorizedRetries < 15 {
				unauthorizedRetries++
			} else {
				return fmt.Errorf("wait for %s: %w", description, err)
			}
		} else {
			unauthorizedRetries = 0
		}
		if err == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for %s", timeout, description)
		case <-ticker.C:
		}
	}
}

func (a *App) step(message string) {
	a.StepIndex++
	_, _ = fmt.Fprintf(a.Out, "\n[%d] %s...\n", a.StepIndex, message)
}

