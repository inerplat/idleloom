package wirekubecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const CompatibleVersion = "v0.0.15"

const compatibleSchemaVersion = "v1alpha1"

type Plan struct {
	SchemaVersion   string   `json:"schemaVersion"`
	Context         string   `json:"context"`
	WireKubeVersion string   `json:"wireKubeVersion"`
	Image           string   `json:"image"`
	Relay           string   `json:"relay"`
	RelayUDP        bool     `json:"relayUDP"`
	MeshCIDR        string   `json:"meshCIDR"`
	NodeAddresses   string   `json:"nodeAddresses"`
	Impact          []string `json:"infrastructureImpact"`
	Warnings        []string `json:"warnings"`
	Detection       struct {
		KubernetesVersion string `json:"kubernetesVersion"`
		Provider          string `json:"provider"`
		CNI               string `json:"cni"`
	} `json:"detection"`
}

type Result struct {
	SchemaVersion  string `json:"schemaVersion"`
	Operation      string `json:"operation"`
	InstallationID string `json:"installationID"`
	Ready          bool   `json:"ready"`
}

type VersionInfo struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	BuildDate    string `json:"buildDate"`
	DefaultImage string `json:"defaultImage"`
}

type Runner func(context.Context, string, ...string) ([]byte, []byte, error)

type Client struct {
	Binary          string
	ExpectedVersion string
	Kubeconfig      string
	Context         string
	Timeout         time.Duration
	Run             Runner
}

func (c Client) Version(ctx context.Context) (VersionInfo, error) {
	var info VersionInfo
	if err := c.runJSON(ctx, &info, "version", "--output", "json"); err != nil {
		return VersionInfo{}, err
	}
	return info, nil
}

func (c Client) Plan(ctx context.Context) (Plan, error) {
	args := []string{
		"install",
		"--relay", "load-balancer",
		"--relay-udp=false",
		"--node-addresses", "internal-ip",
		"--dry-run",
		"--output", "json",
	}
	var plan Plan
	if err := c.runJSON(ctx, &plan, c.clusterArgs(args)...); err != nil {
		return Plan{}, err
	}
	if plan.SchemaVersion != compatibleSchemaVersion {
		return Plan{}, fmt.Errorf("WireKube returned unsupported plan schema %q", plan.SchemaVersion)
	}
	if plan.WireKubeVersion != c.compatibleVersion() {
		return Plan{}, fmt.Errorf("WireKube plan version %q is incompatible; Idleloom requires %s", plan.WireKubeVersion, c.compatibleVersion())
	}
	if plan.Relay != "load-balancer" || plan.RelayUDP || plan.NodeAddresses != "internal-ip" {
		return Plan{}, fmt.Errorf("WireKube returned an incompatible connected-host plan")
	}
	if strings.TrimSpace(plan.MeshCIDR) == "" || !hasSHA256Digest(plan.Image) {
		return Plan{}, fmt.Errorf("WireKube returned an incomplete installation plan")
	}
	return plan, nil
}

func (c Client) Install(ctx context.Context, plan Plan) (Result, error) {
	if strings.TrimSpace(plan.MeshCIDR) == "" {
		return Result{}, fmt.Errorf("WireKube installation plan has no mesh CIDR")
	}
	args := []string{
		"install",
		"--relay", "load-balancer",
		"--relay-udp=false",
		"--mesh-cidr", plan.MeshCIDR,
		"--node-addresses", "internal-ip",
		"--yes",
		"--output", "json",
	}
	var result Result
	if err := c.runJSON(ctx, &result, c.clusterArgs(args)...); err != nil {
		return Result{}, err
	}
	if result.SchemaVersion != compatibleSchemaVersion || result.Operation != "install" || !result.Ready || result.InstallationID == "" {
		return Result{}, fmt.Errorf("WireKube installation completed without a ready installation identity")
	}
	return result, nil
}

func (c Client) compatibleVersion() string {
	if c.ExpectedVersion != "" {
		return c.ExpectedVersion
	}
	return CompatibleVersion
}

func (c Client) clusterArgs(args []string) []string {
	result := append([]string(nil), args...)
	if c.Kubeconfig != "" {
		result = append(result, "--kubeconfig", c.Kubeconfig)
	}
	if c.Context != "" {
		result = append(result, "--context", c.Context)
	}
	if c.Timeout > 0 {
		result = append(result, "--timeout", c.Timeout.String())
	}
	return result
}

func (c Client) runJSON(ctx context.Context, target any, args ...string) error {
	if c.Binary == "" {
		return fmt.Errorf("wirekubectl binary is required")
	}
	runner := c.Run
	if runner == nil {
		runner = runCommand
	}
	stdout, stderr, err := runner(ctx, c.Binary, args...)
	if err != nil {
		message := strings.TrimSpace(string(stderr))
		if message == "" {
			message = strings.TrimSpace(string(stdout))
		}
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("wirekubectl %s failed: %s", args[0], message)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode wirekubectl %s output: %w", args[0], err)
	}
	return nil
}

func runCommand(ctx context.Context, binary string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
