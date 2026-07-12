package wirekube

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	RuntimeStatusVersion = 1
	runtimeFileName      = "wirekube-runtime.json"
)

type RuntimeStatus struct {
	Version           int       `json:"version"`
	InstanceID        string    `json:"instanceID"`
	ProcessID         int       `json:"processID"`
	PeerUID           types.UID `json:"peerUID"`
	InterfaceName     string    `json:"interfaceName,omitempty"`
	LastHandshakeTime time.Time `json:"lastHandshakeTime,omitempty"`
	BytesReceived     int64     `json:"bytesReceived,omitempty"`
	BytesSent         int64     `json:"bytesSent,omitempty"`
	ObservedAt        time.Time `json:"observedAt"`
	Error             string    `json:"error,omitempty"`
}

func DefaultRuntimeDirectory(state State) (string, error) {
	if state.PeerUID == "" {
		return "", fmt.Errorf("WireKube leaf state has no peer UID")
	}
	digest := sha256.Sum256([]byte(state.PeerUID))
	return filepath.Join("/var/run", "idleloom", fmt.Sprintf("%x", digest[:16])), nil
}

func WriteRuntimeStatus(directory string, status RuntimeStatus) error {
	if status.Version != RuntimeStatusVersion || status.InstanceID == "" || status.ProcessID <= 0 || status.PeerUID == "" || status.ObservedAt.IsZero() {
		return fmt.Errorf("WireKube runtime status is incomplete")
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return writeReadable(filepath.Join(directory, runtimeFileName), append(data, '\n'))
}

func ReadRuntimeStatus(directory string, state State, now time.Time, maximumAge time.Duration) (nativev1alpha1.HostConnectivityStatus, error) {
	connectivity := nativev1alpha1.HostConnectivityStatus{
		Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, Provider: nativev1alpha1.ConnectivityProviderWireKube,
		Transport: nativev1alpha1.ConnectivityTransportRelay, PeerName: state.PeerName,
		Address: state.AssignedMeshIP,
	}
	runtimeStatus, err := readRuntimeStatus(directory)
	if err != nil {
		return connectivity, fmt.Errorf("read WireKube runtime status: %w", err)
	}
	if runtimeStatus.PeerUID != state.PeerUID {
		return connectivity, fmt.Errorf("WireKube runtime status belongs to a different peer")
	}
	if maximumAge <= 0 {
		maximumAge = 15 * time.Second
	}
	age := now.Sub(runtimeStatus.ObservedAt)
	if age < -nativev1alpha1.HeartbeatClockSkewAllowance || age > maximumAge {
		return connectivity, fmt.Errorf("WireKube runtime status is stale")
	}
	connectivity.InterfaceName = runtimeStatus.InterfaceName
	if !runtimeStatus.LastHandshakeTime.IsZero() {
		handshake := metav1.NewMicroTime(runtimeStatus.LastHandshakeTime)
		connectivity.LastHandshakeTime = &handshake
	}
	if runtimeStatus.Error != "" {
		return connectivity, fmt.Errorf("WireKube runtime: %s", runtimeStatus.Error)
	}
	return connectivity, nil
}

func RuntimeStatusIsActive(directory string, state State) (bool, error) {
	held, err := RuntimeLockIsHeld(directory)
	if err != nil {
		return false, err
	}
	if !held {
		return false, nil
	}
	status, err := readRuntimeStatus(directory)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read WireKube runtime status: %w", err)
	}
	if status.PeerUID != state.PeerUID {
		return false, fmt.Errorf("WireKube runtime status belongs to a different peer")
	}
	return true, nil
}

func RemoveRuntimeStatus(directory, instanceID string) error {
	path := filepath.Join(directory, runtimeFileName)
	if instanceID != "" {
		status, err := readRuntimeStatus(directory)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if status.InstanceID != instanceID {
			return fmt.Errorf("refusing to remove runtime status owned by another connectivity instance")
		}
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func readRuntimeStatus(directory string) (RuntimeStatus, error) {
	data, err := os.ReadFile(filepath.Join(directory, runtimeFileName))
	if err != nil {
		return RuntimeStatus{}, err
	}
	var status RuntimeStatus
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return RuntimeStatus{}, fmt.Errorf("decode WireKube runtime status: %w", err)
	}
	if status.Version != RuntimeStatusVersion || status.InstanceID == "" || status.ProcessID <= 0 || status.PeerUID == "" || status.ObservedAt.IsZero() {
		return RuntimeStatus{}, fmt.Errorf("WireKube runtime status is incomplete")
	}
	return status, nil
}

func writeReadable(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(name), ".wirekube-runtime-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}
