package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inerplat/idleloom/internal/cdi"
	"github.com/inerplat/idleloom/internal/config"
	"github.com/inerplat/idleloom/internal/discovery"
	"github.com/inerplat/idleloom/internal/driver"
	"github.com/inerplat/idleloom/internal/health"
	"github.com/inerplat/idleloom/internal/state"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

func main() {
	klog.InitFlags(nil)
	cfg := config.BindFlags(flag.CommandLine)
	flag.Parse()
	cfg.Finalize()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(2)
	}
	if err := run(*cfg); err != nil {
		fmt.Fprintf(os.Stderr, "dra-node failed: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx = klog.NewContext(ctx, klog.Background())

	for _, dir := range []string{cfg.PluginDir, cfg.RegistrarDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
	}

	clientConfig, err := buildClientConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		return err
	}
	cdiWriter, err := cdi.NewWriter(cfg.CDIDir, cfg.DriverName)
	if err != nil {
		return err
	}
	prober := health.CommandProbe{
		Command:  cfg.ProbeCommand,
		Args:     cfg.ProbeArguments(),
		Contains: cfg.ProbeContains,
		Timeout:  cfg.ProbeTimeout,
	}
	plugin := driver.NewPlugin(cfg.DriverName, cfg.NodeName, nil, "", prober, store)
	helper, err := kubeletplugin.Start(
		ctx,
		plugin,
		kubeletplugin.DriverName(cfg.DriverName),
		kubeletplugin.NodeName(cfg.NodeName),
		kubeletplugin.KubeClient(kubeClient),
		kubeletplugin.PluginDataDirectoryPath(cfg.PluginDir),
		kubeletplugin.RegistrarDirectoryPath(cfg.RegistrarDir),
		kubeletplugin.NodeV1(true),
		kubeletplugin.NodeV1beta1(false),
	)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	defer helper.Stop()

	discoverer := discovery.Discoverer{
		DRIDir:         cfg.DRIDir,
		SysfsRoot:      cfg.SysfsRoot,
		ExpectedDriver: cfg.ExpectedDriver,
	}
	reconcile := func() {
		device, healthy := reconcileDevice(ctx, discoverer, cdiWriter, prober, plugin)
		resources := driver.DesiredResources(cfg.DriverName, cfg.NodeName, device, healthy)
		if err := helper.PublishResources(ctx, resources); err != nil {
			klog.FromContext(ctx).Error(err, "publish ResourceSlice")
		}
	}
	reconcile()

	ticker := time.NewTicker(cfg.HealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			reconcile()
		}
	}
}

func reconcileDevice(ctx context.Context, discoverer discovery.Discoverer, cdiWriter *cdi.Writer, prober health.Prober, plugin *driver.Plugin) (*discovery.Device, bool) {
	logger := klog.FromContext(ctx)
	devices, err := discoverer.Discover()
	if err != nil {
		logger.Error(err, "discover render devices")
		plugin.SetDevice(nil, "")
		return nil, false
	}
	if len(devices) == 0 {
		logger.V(2).Info("No virtio_gpu render device discovered")
		plugin.SetDevice(nil, "")
		return nil, false
	}
	if len(devices) > 1 {
		logger.Error(fmt.Errorf("found %d devices", len(devices)), "Initial driver supports exactly one render device")
		plugin.SetDevice(nil, "")
		return nil, false
	}

	device := devices[0]
	cdiID, err := cdiWriter.Ensure(device)
	if err != nil {
		logger.Error(err, "write CDI spec", "device", device.Name)
		plugin.SetDevice(nil, "")
		return nil, false
	}
	plugin.SetDevice(&device, cdiID)
	result := prober.Probe(ctx, device)
	if !result.Healthy {
		logger.Error(result.Err, "Vulkan health probe failed", "device", device.Name, "duration", result.Duration)
		return &device, false
	}
	logger.V(2).Info("Apple Vulkan device is healthy", "device", device.Name, "path", device.Path, "duration", result.Duration)
	return &device, true
}

func buildClientConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
	}
	return cfg, nil
}
