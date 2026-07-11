package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const DevelopmentDriverName = "gpu.apple-vulkan.example"

type Config struct {
	DriverName       string
	NodeName         string
	Kubeconfig       string
	DRIDir           string
	SysfsRoot        string
	PluginDir        string
	RegistrarDir     string
	CDIDir           string
	StateFile        string
	ProbeCommand     string
	ProbeArgs        string
	ProbeContains    string
	ProbeTimeout     time.Duration
	HealthInterval   time.Duration
	ExpectedDriver   string
	AllowDevelopment bool
}

func BindFlags(fs *flag.FlagSet) *Config {
	nodeName := os.Getenv("NODE_NAME")
	cfg := &Config{}
	fs.StringVar(&cfg.DriverName, "driver-name", DevelopmentDriverName, "DRA driver name")
	fs.StringVar(&cfg.NodeName, "node-name", nodeName, "Kubernetes node name (defaults to NODE_NAME)")
	fs.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "kubeconfig path; empty uses in-cluster config")
	fs.StringVar(&cfg.DRIDir, "dri-dir", "/dev/dri", "DRM device directory")
	fs.StringVar(&cfg.SysfsRoot, "sysfs-root", "/sys", "sysfs root")
	fs.StringVar(&cfg.PluginDir, "plugin-dir", "", "kubelet plugin data directory")
	fs.StringVar(&cfg.RegistrarDir, "registrar-dir", "/var/lib/kubelet/plugins_registry", "kubelet plugin registry directory")
	fs.StringVar(&cfg.CDIDir, "cdi-dir", "/var/run/cdi", "CDI spec directory")
	fs.StringVar(&cfg.StateFile, "state-file", "", "durable prepared-Claim state file")
	fs.StringVar(&cfg.ProbeCommand, "probe-command", "apple-vulkan-probe", "health probe executable")
	fs.StringVar(&cfg.ProbeArgs, "probe-args", "", "space-separated health probe arguments")
	fs.StringVar(&cfg.ProbeContains, "probe-contains", "venus compute probe ok", "case-insensitive text required in probe output; empty disables the check")
	fs.DurationVar(&cfg.ProbeTimeout, "probe-timeout", 15*time.Second, "health probe timeout")
	fs.DurationVar(&cfg.HealthInterval, "health-interval", 30*time.Second, "background health probe interval")
	fs.StringVar(&cfg.ExpectedDriver, "expected-drm-driver", "virtio_gpu", "required sysfs DRM driver")
	fs.BoolVar(&cfg.AllowDevelopment, "allow-development-driver", false, "allow the reserved development driver name")
	return cfg
}

func (c *Config) Finalize() {
	if c.PluginDir == "" {
		c.PluginDir = filepath.Join("/var/lib/kubelet/plugins", c.DriverName)
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Join(c.PluginDir, "prepared-claims.json")
	}
}

func (c Config) Validate() error {
	var errs []error
	if problems := validation.IsDNS1123Subdomain(c.DriverName); len(problems) > 0 {
		errs = append(errs, fmt.Errorf("invalid driver name %q: %s", c.DriverName, strings.Join(problems, ", ")))
	}
	if c.DriverName == DevelopmentDriverName && !c.AllowDevelopment {
		errs = append(errs, fmt.Errorf("driver name %q is reserved for development; set --allow-development-driver or choose an owned domain", c.DriverName))
	}
	if c.NodeName == "" {
		errs = append(errs, errors.New("node name is required"))
	} else if problems := validation.IsDNS1123Subdomain(c.NodeName); len(problems) > 0 {
		errs = append(errs, fmt.Errorf("invalid node name %q: %s", c.NodeName, strings.Join(problems, ", ")))
	}
	if c.ProbeTimeout <= 0 {
		errs = append(errs, errors.New("probe timeout must be positive"))
	}
	if c.HealthInterval <= 0 {
		errs = append(errs, errors.New("health interval must be positive"))
	}
	return errors.Join(errs...)
}

func (c Config) ProbeArguments() []string {
	return strings.Fields(c.ProbeArgs)
}
