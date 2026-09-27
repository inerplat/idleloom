package idleloom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/inerplat/idleloom/internal/filelock"

	"github.com/inerplat/idleloom/internal/syncdir"
)

type State struct {
	NodeName             string `json:"nodeName"`
	KubeconfigPath       string `json:"kubeconfigPath"`
	Context              string `json:"context"`
	Network              string `json:"network"`
	Taint                string `json:"taint,omitempty"`
	TaintConfigured      bool   `json:"taintConfigured,omitempty"`
	TokenTTLSeconds      int64  `json:"tokenTTLSeconds,omitempty"`
	NetworkLease         string `json:"networkLease,omitempty"`
	NetworkLeaseUID      string `json:"networkLeaseUID,omitempty"`
	NetworkReservationID string `json:"networkReservationID,omitempty"`
	// MeshAddressClaimed records that this worker holds a cluster-wide claim
	// on its mesh address, so teardown knows to hand it back. In-place workers
	// take their node address from the WireKube mesh rather than from a
	// private subnet, so they hold a claim instead of a network reservation.
	MeshAddressClaimed bool `json:"meshAddressClaimed,omitempty"`
	// Distribution records the WSL2 distribution this worker was enrolled on,
	// so later lifecycle commands act on it rather than the machine's current
	// default distribution.
	Distribution string       `json:"distribution,omitempty"`
	Runtime      RuntimeState `json:"runtime"`
	Phase        string       `json:"phase"`
	CreatedAt    time.Time    `json:"createdAt"`
	// RegistryMirrors and the credential provider host paths are persisted so
	// an interrupted enrollment can rebuild the worker bundle on resume. Only
	// paths are stored, never secret file contents.
	RegistryMirrors          []RegistryMirror `json:"registryMirrors,omitempty"`
	CredentialProviderBins   []string         `json:"credentialProviderBins,omitempty"`
	CredentialProviderConfig string           `json:"credentialProviderConfig,omitempty"`
	CredentialProviderEnv    string           `json:"credentialProviderEnv,omitempty"`
}

type stateLock struct {
	file *os.File
}

func AcquireStateLock(ctx context.Context, statePath string) (*stateLock, error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, fmt.Errorf("create state lock directory: %w", err)
	}
	file, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, err := filelock.TryLock(file)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("lock Idleloom state: %w", err), file.Close())
		}
		if locked {
			return &stateLock{file: file}, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("wait for Idleloom state lock: %w", ctx.Err()), file.Close())
		case <-ticker.C:
		}
	}
}

func (l *stateLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := filelock.Unlock(l.file)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

type RuntimeState struct {
	NodeName      string `json:"nodeName"`
	RuntimeDir    string `json:"runtimeDir"`
	RootDisk      string `json:"rootDisk"`
	SeedISO       string `json:"seedISO"`
	SSHPrivateKey string `json:"sshPrivateKey"`
	MACAddress    string `json:"macAddress"`
	Subnet        string `json:"subnet"`
	GatewayIP     string `json:"gatewayIP"`
	GuestIP       string `json:"guestIP"`
	HostIP        string `json:"hostIP"`
	SSHPort       int    `json:"sshPort"`
	CPUs          int    `json:"cpus"`
	MemoryMB      int    `json:"memoryMB"`
	DiskMB        int    `json:"diskMB"`
	Planned       bool   `json:"planned,omitempty"`
}

// HoldsNetworkReservation reports whether this worker owns a cluster-wide
// subnet lease. Only the VM-provisioning backends take one: an in-place worker
// derives its node address from the WireKube mesh, which needs no lease
// because the address is a function of the node name.
func (s State) HoldsNetworkReservation() bool {
	return s.NetworkReservationID != ""
}

// HoldsMeshAddressClaim reports whether teardown has a mesh address to release.
func (s State) HoldsMeshAddressClaim() bool {
	return s.MeshAddressClaimed
}

func DefaultStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".idleloom", "state.json"), nil
}

func SaveState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := atomicWriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".idleloom-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		return errors.Join(fmt.Errorf("set temporary file permissions: %w", err), temporary.Close())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write temporary file: %w", err), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temporary file: %w", err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	return syncdir.Sync(dir)
}

func EnsureStatePathAvailable(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("an Idleloom worker already exists (state at %s); check it with \"idlectl status\", or pass a different --state path", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect state path %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("state path %s is not writable: %w", path, err)
	}
	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close state path probe: %w", closeErr)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove state path probe: %w", err)
	}
	return nil
}

func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, fmt.Errorf("no Idleloom worker exists on this host (state file %s not found); create one with \"idlectl create worker NAME\". If this is a Native Metal host, use \"idlectl delete host NAME\" instead", path)
		}
		return State{}, fmt.Errorf("read state %s: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode state %s: %w", path, err)
	}
	if state.NodeName == "" || state.Runtime.NodeName != state.NodeName {
		return State{}, fmt.Errorf("state %s has inconsistent node ownership: state=%q runtime=%q", path, state.NodeName, state.Runtime.NodeName)
	}
	if state.TaintConfigured {
		if err := validateTaint(state.Taint); err != nil {
			return State{}, fmt.Errorf("state %s has an invalid taint: %w", path, err)
		}
	} else if state.Taint != "" {
		return State{}, fmt.Errorf("state %s has taint data without a configuration marker", path)
	}
	if state.TokenTTLSeconds < 0 {
		return State{}, fmt.Errorf("state %s has a negative bootstrap token lifetime", path)
	}
	return state, nil
}
