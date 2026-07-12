package wirekube

import (
	"os"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
)

func TestRuntimeStatusReportsConnectedLeafWithoutExposingSecrets(t *testing.T) {
	directory := t.TempDir()
	state := testTunnelState(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := WriteRuntimeStatus(directory, RuntimeStatus{
		Version: RuntimeStatusVersion, InstanceID: "instance-one", ProcessID: os.Getpid(),
		PeerUID: state.PeerUID, InterfaceName: "utun9",
		LastHandshakeTime: now.Add(-time.Second), BytesReceived: 10, BytesSent: 20, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := ReadRuntimeStatus(directory, state, now.Add(time.Second), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if status.Mode != nativev1alpha1.ConnectivityModeWireKubeLeaf || status.InterfaceName != "utun9" || status.LastHandshakeTime == nil {
		t.Fatalf("status = %#v", status)
	}
}

func TestRuntimeStatusRejectsStaleOrDifferentPeer(t *testing.T) {
	directory := t.TempDir()
	state := testTunnelState(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := WriteRuntimeStatus(directory, RuntimeStatus{
		Version: RuntimeStatusVersion, InstanceID: "instance-one", ProcessID: os.Getpid(),
		PeerUID: "different-peer", ObservedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntimeStatus(directory, state, now, 15*time.Second); err == nil {
		t.Fatal("ReadRuntimeStatus accepted a stale status for another peer")
	}
}

func TestRemoveRuntimeStatusRequiresMatchingInstance(t *testing.T) {
	directory := t.TempDir()
	state := testTunnelState(t)
	if err := WriteRuntimeStatus(directory, RuntimeStatus{
		Version: RuntimeStatusVersion, InstanceID: "instance-one", ProcessID: os.Getpid(),
		PeerUID: state.PeerUID, ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRuntimeStatus(directory, "instance-two"); err == nil {
		t.Fatal("RemoveRuntimeStatus removed another runner's receipt")
	}
	if err := RemoveRuntimeStatus(directory, "instance-one"); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStatusDetectsLiveProcessEvenWhenReceiptIsStale(t *testing.T) {
	directory := t.TempDir()
	state := testTunnelState(t)
	lock, err := AcquireRuntimeLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := WriteRuntimeStatus(directory, RuntimeStatus{
		Version: RuntimeStatusVersion, InstanceID: lock.InstanceID, ProcessID: os.Getpid(),
		PeerUID: state.PeerUID, ObservedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	active, err := RuntimeStatusIsActive(directory, state)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Fatal("live runtime process was treated as inactive because its receipt was stale")
	}
}
