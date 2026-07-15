package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type WireKubeStatus struct {
	Installed             bool
	IncludeNodeInternalIP bool
	AgentNamespace        string
	AgentName             string
	ReadyPeers            int64
}

func CheckWireKube(ctx context.Context, client kubernetes.Interface) (WireKubeStatus, error) {
	return checkWireKube(ctx, client, true)
}

func checkWireKubeForRegistration(ctx context.Context, client kubernetes.Interface) (WireKubeStatus, error) {
	return checkWireKube(ctx, client, false)
}

func checkWireKube(ctx context.Context, client kubernetes.Interface, requireReadyPeer bool) (WireKubeStatus, error) {
	var status WireKubeStatus
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubemeshes/default").Do(ctx).Raw()
	if err != nil {
		return status, fmt.Errorf("the WireKube mesh is not available: %w", err)
	}
	var mesh struct {
		Spec struct {
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
	if err := validateWireKubeStatus(status, requireReadyPeer); err != nil {
		return status, err
	}
	return status, nil
}

func validateWireKubeStatus(status WireKubeStatus, requireReadyPeer bool) error {
	if status.AgentName == "" {
		return fmt.Errorf("the WireKubeMesh exists but no WireKube agent DaemonSet was found")
	}
	if !status.IncludeNodeInternalIP {
		return fmt.Errorf("the WireKubeMesh default must set spec.autoAllowedIPs.includeNodeInternalIP=true")
	}
	if requireReadyPeer && status.ReadyPeers == 0 {
		return fmt.Errorf("the WireKube installation has no ready ingress peers")
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
