package idleloom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
	// Distribution names the WSL2 distribution to enroll on a Windows host.
	Distribution string
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
	DownloadKubelet          func(context.Context, string, string) (string, error)
	SaveImage                func(context.Context, string, []string, string) error
	ApproveKubeletServingCSR func(context.Context, *Cluster, string, string, time.Time, bool, time.Duration) error
	StartMaintainer          func(context.Context, string, io.Writer) error
	StepIndex                int
}

// NewApp builds the worker application with the backend for this host:
// krunkit on macOS, the host itself on Linux, and a WSL2 distribution on
// Windows. WorkerOptions carries the settings only some backends read.
func NewApp(out, errOut io.Writer, opts WorkerOptions) *App {
	return &App{
		Out:       out,
		Err:       errOut,
		Now:       time.Now,
		Runtime:   defaultRuntime(ExecRunner{}, out, errOut, opts),
		SaveImage: SaveImage,
	}
}

// WorkerOptions are host-level settings chosen before a cluster is contacted.
type WorkerOptions struct {
	// Distribution names the WSL2 distribution to enrol. It is ignored off
	// Windows, and defaults to the machine's default distribution.
	Distribution string
}

// ProvisionsVM reports whether this host's backend builds a virtual machine.
// The CPU, memory, and disk settings describe that machine, so they are only
// meaningful — and only worth asking about — when there is one.
func (a *App) ProvisionsVM() bool {
	return a.Runtime.Backend().ProvisionsVM()
}

func (a *App) Init(ctx context.Context, opts InitOptions) error {
	if err := validateInitOptions(opts, a.Runtime.Backend()); err != nil {
		return err
	}
	mirrors, mirrorWarnings, err := parseRegistryMirrors(opts.RegistryMirrors)
	if err != nil {
		return err
	}
	for _, warning := range mirrorWarnings {
		_, _ = fmt.Fprintf(a.Err, "warning: %s\n", warning)
	}
	if err := validateCredentialProviders(opts.CredentialProviderBins, opts.CredentialProviderConfig, opts.CredentialProviderEnv, a.Runtime.GuestArch()); err != nil {
		return err
	}

	a.step(a.preflightStepMessage())
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
		wireKube, err = CheckWireKube(ctx, cluster.Client)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Out, "  Agent:   %s/%s\n", wireKube.AgentNamespace, wireKube.AgentName)
		_, _ = fmt.Fprintf(a.Out, "  Peers:   %d ready\n", wireKube.ReadyPeers)
		if wireKube.ReadyPeers == 0 {
			if opts.SkipWait {
				_, _ = fmt.Fprintln(a.Err, "warning: WireKube has no ready ingress peers; the registered worker will remain cordoned until \"idlectl start worker\" succeeds")
			} else {
				_, _ = fmt.Fprintln(a.Err, "warning: WireKube has no ready ingress peers yet; this worker should become the first once its agent connects — if the wait below times out, inspect the WireKubeMesh relay status")
			}
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
		// The address is the one thing a dry run can get wrong without
		// telling anybody: an in-place worker's node IP comes from the mesh,
		// and on a mesh that does not arbitrate addresses a name collision is
		// a hard failure that would otherwise only surface halfway through a
		// real enrollment.
		if !a.Runtime.Backend().ProvisionsVM() {
			a.step("Checking the worker mesh address")
			if err := a.previewMeshAddress(ctx, cluster, opts.NodeName, wireKube); err != nil {
				return err
			}
		}
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
		NodeName:       opts.NodeName,
		KubeconfigPath: cluster.KubeconfigPath,
		Context:        cluster.Context,
		Network:        opts.Network,
		// What the runtime bound to, which is the resolved default when the
		// caller named no distribution. Recording the empty string would let
		// a later delete follow whatever the default had become.
		Distribution:    a.Runtime.Environment(),
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
	var runtimeNetwork RuntimeNetwork
	if a.Runtime.Backend().ProvisionsVM() {
		reservationID, err := NewNetworkReservationID()
		if err != nil {
			return err
		}
		state.NetworkReservationID = reservationID
		if err := SaveState(statePath, state); err != nil {
			return errors.Join(err, removeStateFile(statePath))
		}

		a.step("Reserving an isolated worker network")
		network, networkLease, networkLeaseUID, err := ReserveRuntimeNetwork(ctx, cluster.Client, opts.NodeName, state.NetworkReservationID)
		if err != nil {
			return fmt.Errorf("%w; reservation intent was saved to %s for recovery", err, statePath)
		}
		runtimeNetwork = network
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
	} else {
		// An in-place worker shares a network stack Idleloom does not own, so
		// its address cannot come from a private subnet. WireKube already
		// derives a unique overlay address from the node name; taking the node
		// IP from there keeps the address stable, collision-checked, and
		// routable by every mesh peer without a cluster-wide lease.
		a.step("Deriving the worker mesh address")
		mesh, err := ReserveMeshAddress(ctx, cluster.Client, opts.NodeName, wireKube)
		if err != nil {
			return errors.Join(err, removeStateFile(statePath))
		}
		if mesh.Moved {
			_, _ = fmt.Fprintf(a.Err, "warning: the mesh address for node name %q was already taken; WireKube assigned %s instead\n", opts.NodeName, mesh.Address)
		}
		runtimeNetwork = RuntimeNetwork{GuestIP: mesh.Address, Subnet: wireKube.MeshCIDR}
		state.Runtime = RuntimeState{NodeName: opts.NodeName, GuestIP: mesh.Address, Subnet: wireKube.MeshCIDR}
		state.MeshAddressClaimed = mesh.Claimed
		if err := SaveState(statePath, state); err != nil {
			// The claim is already made; drop it rather than leaving an
			// address held by a worker whose state file never landed.
			return errors.Join(err, releaseWorkerReservations(cluster, state, wireKube), removeStateFile(statePath))
		}
	}
	_, _ = fmt.Fprintf(a.Out, "  Node IP: %s (%s)\n", runtimeNetwork.GuestIP, runtimeNetwork.Subnet)

	plannedRuntime, err := a.Runtime.Plan(ctx, RuntimeConfig{
		NodeName: opts.NodeName, CPUs: opts.CPUs, MemoryMB: opts.MemoryMB,
		DiskMB: opts.DiskMB, RuntimeDir: opts.RuntimeDir, Network: runtimeNetwork,
	})
	if err != nil {
		if releaseErr := releaseWorkerReservations(cluster, state, wireKube); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release the worker reservation: %w; recovery state remains at %s", releaseErr, statePath))
		}
		return errors.Join(err, removeStateFile(statePath))
	}
	state.Runtime = plannedRuntime
	if err := SaveState(statePath, state); err != nil {
		if releaseErr := releaseWorkerReservations(cluster, state, wireKube); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("release the worker reservation: %w; recovery state remains at %s", releaseErr, statePath))
		}
		return errors.Join(err, removeStateFile(statePath))
	}

	a.step(a.createStepMessage())
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

	bundlePath, cleanupBundle, err := CreateWorkerBundle(a.workerBundleConfig(state, cluster, token.Value, kubeletPath))
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
	if err := labelNode(ctx, cluster, opts.NodeName, opts.Network, a.Runtime.Backend()); err != nil {
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

// ClusterOverride carries explicit --kubeconfig/--context values. Explicit
// values always win over the state file, mirroring kubectl: flags select the
// cluster per invocation and are never persisted.
type ClusterOverride struct {
	KubeconfigPath string
	Context        string
}

// resolveClusterSource picks the kubeconfig and context used to reach the
// worker's cluster. Each override field wins independently over the value
// recorded in the state file at create time, like kubectl's flags.
func resolveClusterSource(state State, override ClusterOverride) (kubeconfig, kubeContext string) {
	kubeconfig = state.KubeconfigPath
	if override.KubeconfigPath != "" {
		kubeconfig = override.KubeconfigPath
	}
	kubeContext = state.Context
	if override.Context != "" {
		kubeContext = override.Context
	}
	return kubeconfig, kubeContext
}

func (a *App) Start(ctx context.Context, statePath string, override ClusterOverride, timeout time.Duration) error {
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
	kubeconfig, kubeContext := resolveClusterSource(state, override)
	if kubeconfig != state.KubeconfigPath || kubeContext != state.Context {
		_, _ = fmt.Fprintf(a.Err, "warning: the background certificate maintainer keeps using the kubeconfig recorded at create (%s, context %q); recreate the worker if that kubeconfig no longer works\n", state.KubeconfigPath, state.Context)
	}
	cluster, err := LoadCluster(ctx, kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	{
		if err := ValidateNetworkReservationIfHeld(ctx, cluster.Client, state); err != nil {
			return err
		}
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
	a.step(a.lifecycleStepMessage("Starting", false))
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
		return fmt.Errorf("worker enrollment stopped before %s was prepared; delete the local state with \"idlectl delete worker %s --local-only --force --state %s\", then run \"idlectl create worker\" again", a.plannedSubject(), state.NodeName, statePath)
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
		if err := validateCredentialProviders(state.CredentialProviderBins, state.CredentialProviderConfig, state.CredentialProviderEnv, a.Runtime.GuestArch()); err != nil {
			return fmt.Errorf("cannot rebuild the interrupted worker bundle: %w", err)
		}
		bundlePath, cleanupBundle, err := CreateWorkerBundle(a.workerBundleConfig(*state, cluster, token.Value, kubeletPath))
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
	if err := labelNode(ctx, cluster, state.NodeName, state.Network, a.Runtime.Backend()); err != nil {
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

// preflightStepMessage names the preflight step for the active backend.
func (a *App) preflightStepMessage() string {
	switch a.Runtime.Backend() {
	case RuntimeKrunkit:
		return "Checking the Apple Silicon host"
	case RuntimeWSL2:
		return "Checking the WSL2 environment"
	default:
		return "Checking the host"
	}
}

// workerBundleConfig describes the bundle for a worker.
//
// Enrollment and the resume path both install one, and they have to agree on
// every field. They did not: the resume path rebuilt the bundle without the
// node address, so a worker that finished enrolling through a resume
// re-registered with its own detected address instead of the mesh one. Both
// now read the same saved state.
func (a *App) workerBundleConfig(state State, cluster *Cluster, token, kubeletPath string) BundleConfig {
	config := BundleConfig{
		NodeName:      state.NodeName,
		Taint:         state.Taint,
		Server:        cluster.Server,
		TLSServerName: cluster.TLSServerName,
		CAData:        cluster.CAData,
		Token:         token,
		ClusterDNS:    cluster.ClusterDNS,
		ClusterDomain: cluster.ClusterDomain,
		KubeletPath:   kubeletPath,

		RegistryMirrors:          state.RegistryMirrors,
		CredentialProviderBins:   state.CredentialProviderBins,
		CredentialProviderConfig: state.CredentialProviderConfig,
		CredentialProviderEnv:    state.CredentialProviderEnv,
	}
	// Only the in-place backends pin the address. A krunkit VM owns its
	// subnet and detecting the address in the guest stays correct even if
	// gvproxy ever hands out a different one than was recorded.
	if !a.Runtime.Backend().ProvisionsVM() {
		config.NodeIP = state.Runtime.GuestIP
	}
	return config
}

// plannedSubject names what enrollment would have created, for a message
// about an enrollment that stopped before it did.
func (a *App) plannedSubject() string {
	if a.Runtime.Backend().ProvisionsVM() {
		return "the VM"
	}
	return "the host"
}

// lifecycleStepMessage names a start, stop, or delete step for the active
// backend. Only krunkit has a VM to act on; the in-place backends act on the
// worker's services inside an environment that outlives them. local marks the
// steps that deliberately skip the cluster.
func (a *App) lifecycleStepMessage(verb string, local bool) string {
	subject := "worker"
	if a.Runtime.Backend().ProvisionsVM() {
		subject = "krunkit worker VM"
	}
	if local {
		return verb + " the local " + subject
	}
	return verb + " the " + subject
}

// createStepMessage names the provisioning step for the active backend.
func (a *App) createStepMessage() string {
	switch a.Runtime.Backend() {
	case RuntimeKrunkit:
		return "Creating the krunkit worker VM"
	case RuntimeWSL2:
		return "Preparing the WSL2 worker environment"
	default:
		return "Preparing the host as a worker"
	}
}

func (a *App) downloadKubelet(ctx context.Context, version string) (string, error) {
	arch := a.Runtime.GuestArch()
	if a.DownloadKubelet != nil {
		return a.DownloadKubelet(ctx, version, arch)
	}
	return DownloadKubelet(ctx, version, arch)
}

// LoadImage loads local container image(s) directly into the worker's
// containerd so Pods with imagePullPolicy IfNotPresent or Never can use them
// without a registry. Either refs (exported with a container engine) or a
// pre-saved --archive tar is uploaded and imported.
func (a *App) LoadImage(ctx context.Context, statePath string, refs []string, archivePath, enginePath string) error {
	resolvedPath, err := resolveStatePath(statePath)
	if err != nil {
		return err
	}
	state, err := LoadState(resolvedPath)
	if err != nil {
		return err
	}
	status, err := a.Runtime.Status(ctx, &state.Runtime)
	if err != nil {
		return err
	}
	if status.VM != "running" {
		return fmt.Errorf("worker %s is not running; start it with \"idlectl start worker\" before loading images", state.NodeName)
	}

	tarPath := archivePath
	descriptor := "archive " + archivePath
	if archivePath != "" {
		info, err := os.Stat(archivePath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("image archive %s does not exist", archivePath)
			}
			return fmt.Errorf("inspect image archive %s: %w", archivePath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("image archive %s is not a regular file", archivePath)
		}
	} else {
		if len(refs) == 0 {
			return fmt.Errorf("at least one image reference is required unless --archive is set")
		}
		exportDir, err := os.MkdirTemp("", "idleloom-image-*")
		if err != nil {
			return fmt.Errorf("create image export directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(exportDir) }()
		tarPath = filepath.Join(exportDir, "image.tar")
		if err := a.saveImage(ctx, enginePath, refs, tarPath); err != nil {
			return err
		}
		if err := os.Chmod(tarPath, 0o600); err != nil {
			return fmt.Errorf("protect exported image archive: %w", err)
		}
		descriptor = strings.Join(refs, ", ")
	}

	if err := a.Runtime.LoadImage(ctx, state.Runtime, tarPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nLoaded %s into worker %s.\n", descriptor, state.NodeName)
	return nil
}

func (a *App) saveImage(ctx context.Context, enginePath string, refs []string, dest string) error {
	if a.SaveImage != nil {
		return a.SaveImage(ctx, enginePath, refs, dest)
	}
	return SaveImage(ctx, enginePath, refs, dest)
}

// SaveImage exports the named images into dest as a tar. When enginePath is
// empty it auto-detects a container engine (docker, nerdctl, then podman) from
// PATH; all three support "<engine> save <refs...> -o <dest>".
func SaveImage(ctx context.Context, enginePath string, refs []string, dest string) error {
	engine := enginePath
	if engine == "" {
		detected, err := detectContainerEngine()
		if err != nil {
			return err
		}
		engine = detected
	}
	args := append([]string{"save"}, refs...)
	args = append(args, "-o", dest)
	command := exec.CommandContext(ctx, engine, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("export images with %s: %w: %s", engine, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func detectContainerEngine() (string, error) {
	for _, name := range []string{"docker", "nerdctl", "podman"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no container engine found (looked for docker, nerdctl, podman); install one or pass --archive PATH")
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

func (a *App) Stop(ctx context.Context, statePath string, override ClusterOverride, localOnly bool) error {
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
		a.step(a.lifecycleStepMessage("Stopping", true))
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
	kubeconfig, kubeContext := resolveClusterSource(state, override)
	cluster, err := LoadCluster(ctx, kubeconfig, kubeContext)
	if err != nil {
		return err
	}
	{
		if err := ValidateNetworkReservationIfHeld(ctx, cluster.Client, state); err != nil {
			return err
		}
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
	a.step(a.lifecycleStepMessage("Stopping", false))
	if err := a.Runtime.Stop(ctx, state.Runtime); err != nil {
		return err
	}
	if err := stopMaintainer(resolvedPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Out, "\nIdleloom worker %s is stopped and cordoned.\n", state.NodeName)
	return nil
}

func (a *App) Delete(ctx context.Context, statePath string, override ClusterOverride, force, localOnly bool) error {
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
		a.step(a.lifecycleStepMessage("Deleting", true))
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
	kubeconfig, kubeContext := resolveClusterSource(state, override)
	cluster, err := LoadCluster(ctx, kubeconfig, kubeContext)
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
		a.step(a.lifecycleStepMessage("Deleting", false))
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
	if state.HoldsNetworkReservation() {
		if err := ReleaseRuntimeNetwork(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID); err != nil {
			return err
		}
	}
	if state.HoldsMeshAddressClaim() {
		// WireKube's reaper collects an unheld claim on its own, so a failure
		// here delays the address coming back rather than losing it. Say so
		// and carry on: refusing to finish a teardown over it would leave the
		// operator with a half-deleted worker.
		//
		// Read the mesh rather than check it: an installation that would fail a
		// fresh enrollment can still be holding this worker's address.
		wireKube, err := ReadWireKube(ctx, cluster.Client)
		if err != nil {
			_, _ = fmt.Fprintf(a.Err, "warning: could not read the WireKube mesh to release the address claim for %s: %v; WireKube will reclaim it once the peer is gone\n", state.NodeName, err)
		} else {
			// The WireKubePeer goes when its Node does, but by garbage
			// collection, which is asynchronous. Releasing the address while
			// the peer is still advertising it lets the next enrollment take
			// it and leaves two peers on one /32 until the collector catches
			// up, so wait for the peer to actually go first.
			if err := waitForPeerGone(ctx, cluster.Client, state.NodeName, meshPeerRemovalTimeout); err != nil {
				_, _ = fmt.Fprintf(a.Err, "warning: %v; leaving the mesh address claim for WireKube to reclaim\n", err)
			} else if err := ReleaseMeshAddress(ctx, cluster.Client, state.NodeName, wireKube); err != nil {
				_, _ = fmt.Fprintf(a.Err, "warning: could not release the mesh address claim for %s: %v; WireKube will reclaim it once the peer is gone\n", state.NodeName, err)
			}
		}
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

func validateInitOptions(opts InitOptions, backend RuntimeKind) error {
	if problems := validation.IsDNS1123Subdomain(opts.NodeName); len(problems) > 0 {
		return fmt.Errorf("invalid node name %q: %v", opts.NodeName, problems)
	}
	// Sizing describes a virtual machine Idleloom builds. An in-place worker
	// runs in an environment that already exists and is sized elsewhere — by
	// the host itself, or by .wslconfig — so these bounds do not apply.
	if backend.ProvisionsVM() {
		if opts.CPUs < 2 {
			return fmt.Errorf("at least 2 CPUs are required")
		}
		if opts.MemoryMB < 4096 {
			return fmt.Errorf("at least 4096 MiB of memory is required by the krunkit GPU VM")
		}
		if opts.DiskMB < 6144 {
			return fmt.Errorf("at least 6144 MiB of disk is required")
		}
	}
	if err := validateTaint(opts.Taint); err != nil {
		return err
	}
	if opts.Network != NetworkWireKube {
		return fmt.Errorf("network must be %q; Idleloom reaches workers through the WireKube mesh", NetworkWireKube)
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

func labelNode(ctx context.Context, cluster *Cluster, nodeName, network string, backend RuntimeKind) error {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "idleloom",
		"idleloom-worker":              "true",
		"idleloom-runtime":             string(backend),
	}
	// Only the krunkit VM exposes Apple's GPU through the Vulkan DRA driver.
	// An in-place worker sees whatever the host exposes to Linux directly, so
	// accelerators there are discovered by the cluster's own device plugins
	// rather than claimed by Idleloom.
	if backend == RuntimeKrunkit {
		labels["idleloom-accelerator"] = "apple-vulkan"
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

// previewMeshAddress prints what a dry run can tell the operator about the
// worker's node address.
func (a *App) previewMeshAddress(ctx context.Context, cluster *Cluster, nodeName string, wireKube WireKubeStatus) error {
	preview, err := PreviewMeshAddress(ctx, cluster.Client, nodeName, wireKube)
	if err != nil {
		return err
	}
	if preview.Moved {
		_, _ = fmt.Fprintf(a.Out, "  Node IP: %s is already claimed; WireKube would assign a free address instead\n", preview.Address)
		return nil
	}
	_, _ = fmt.Fprintf(a.Out, "  Node IP: %s (%s)\n", preview.Address, wireKube.MeshCIDR)
	return nil
}

// releaseWorkerReservations hands back whatever cluster-wide reservation this
// worker has taken, on a failure path that is about to delete the state file
// recording it.
//
// A VM backend holds a private subnet and an in-place backend holds a mesh
// address claim; each holds exactly one, so the other call is a no-op. Doing
// only the subnet half, as these paths used to, left an in-place worker's
// address held with nothing left on disk that could ever release it.
func releaseWorkerReservations(cluster *Cluster, state State, wireKube WireKubeStatus) error {
	return errors.Join(
		ReleaseRuntimeNetwork(context.Background(), cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID),
		releaseMeshAddress(cluster, state, wireKube),
	)
}

// releaseMeshAddress drops a claim made moments earlier, for the failure paths
// between claiming the address and having a state file that records it.
func releaseMeshAddress(cluster *Cluster, state State, wireKube WireKubeStatus) error {
	if !state.HoldsMeshAddressClaim() {
		return nil
	}
	// The caller's context is already failing, so this needs one of its own.
	// It is bounded because it runs on the way out of a command that has
	// already gone wrong, and WireKube reclaims the address regardless.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ReleaseMeshAddress(ctx, cluster.Client, state.NodeName, wireKube); err != nil {
		return fmt.Errorf("release the mesh address claim for %s: %w", state.NodeName, err)
	}
	return nil
}
