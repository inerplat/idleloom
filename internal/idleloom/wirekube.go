package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/inerplat/idleloom/internal/meship"
)

type WireKubeStatus struct {
	Installed             bool
	IncludeNodeInternalIP bool
	AgentNamespace        string
	AgentName             string
	ReadyPeers            int64
	// MeshCIDR is the overlay range peers draw their addresses from. The
	// in-place backends derive the worker's node address from it before the
	// node exists, so enrollment needs it up front.
	MeshCIDR string
}

// CheckWireKube validates the WireKube installation structurally: the mesh
// exists, an agent DaemonSet runs, and Node InternalIPs are advertised. It
// deliberately does NOT require a ready ingress peer — on a bootstrapping mesh
// this worker becomes the first one, and waitForWireKube already waits for the
// worker's own peer to connect within the command timeout.
func CheckWireKube(ctx context.Context, client kubernetes.Interface) (WireKubeStatus, error) {
	var status WireKubeStatus
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubemeshes/default").Do(ctx).Raw()
	if err != nil {
		return status, fmt.Errorf("the WireKube mesh is not available: %w", err)
	}
	var mesh struct {
		Spec struct {
			MeshCIDR       string `json:"meshCIDR"`
			AutoAllowedIPs struct {
				IncludeNodeInternalIP bool `json:"includeNodeInternalIP"`
			} `json:"autoAllowedIPs"`
		} `json:"spec"`
		Status struct {
			ReadyPeers int64 `json:"readyPeers"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &mesh); err != nil {
		return status, fmt.Errorf("decode WireKubeMesh: %w", err)
	}
	status.Installed = true
	status.IncludeNodeInternalIP = mesh.Spec.AutoAllowedIPs.IncludeNodeInternalIP
	status.ReadyPeers = mesh.Status.ReadyPeers
	status.MeshCIDR = mesh.Spec.MeshCIDR

	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return status, fmt.Errorf("list DaemonSets while checking WireKube: %w", err)
	}
	for _, daemonSet := range daemonSets.Items {
		if strings.Contains(strings.ToLower(daemonSet.Name), "wirekube") {
			status.AgentNamespace = daemonSet.Namespace
			status.AgentName = daemonSet.Name
			break
		}
	}
	if err := validateWireKubeStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

func validateWireKubeStatus(status WireKubeStatus) error {
	if status.AgentName == "" {
		return fmt.Errorf("the WireKubeMesh exists but no WireKube agent DaemonSet was found")
	}
	if !status.IncludeNodeInternalIP {
		return fmt.Errorf("the WireKubeMesh default must set spec.autoAllowedIPs.includeNodeInternalIP=true")
	}
	return nil
}

func WireKubePeerConnected(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, error) {
	path := "/apis/wirekube.io/v1alpha1/wirekubepeers/" + nodeName
	raw, err := client.Discovery().RESTClient().Get().AbsPath(path).Do(ctx).Raw()
	if err != nil {
		return false, err
	}
	var peer struct {
		Status struct {
			Connected bool `json:"connected"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &peer); err != nil {
		return false, fmt.Errorf("decode WireKubePeer %s: %w", nodeName, err)
	}
	return peer.Status.Connected, nil
}

// ReserveMeshAddress derives the worker's node address from the WireKube mesh
// and confirms no other peer already holds it.
//
// WireKube's allocator is a hash of the node name, so the address needs no
// cluster-wide lease: it is reproducible from the name alone. It is not
// collision-free though — the hash is reduced into the mesh CIDR — so an
// existing peer holding the same address has to be reported rather than
// silently overwritten.
func ReserveMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName, meshCIDR string) (string, error) {
	if meshCIDR == "" {
		return "", fmt.Errorf("the WireKubeMesh does not publish spec.meshCIDR, so the worker node address cannot be derived")
	}
	address, err := meship.AddressForName(nodeName, meshCIDR)
	if err != nil {
		return "", err
	}
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubepeers").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("list WireKube peers to check the worker address: %w", err)
	}
	var peers struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				AllowedIPs []string `json:"allowedIPs"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &peers); err != nil {
		return "", fmt.Errorf("decode WireKube peers: %w", err)
	}
	for _, peer := range peers.Items {
		if peer.Metadata.Name == nodeName {
			continue
		}
		for _, allowed := range peer.Spec.AllowedIPs {
			if strings.TrimSuffix(allowed, "/32") == address {
				return "", fmt.Errorf("the mesh address %s derived from node name %q is already held by WireKubePeer/%s; enrol this worker under a different name", address, nodeName, peer.Metadata.Name)
			}
		}
	}
	return address, nil
}
