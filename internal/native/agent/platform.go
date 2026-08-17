package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Platform interface {
	KrunkitRunning(context.Context) (bool, error)
	AllocatableMemory(context.Context) (resource.Quantity, error)
	MemorySnapshot(context.Context) (MemorySnapshot, error)
	ProcessStartToken(int) (string, error)
	ProcessAlive(int) (bool, error)
	FindRunnerPIDs(context.Context, string, string) ([]int, error)
	KillProcessGroupAndWait(context.Context, int) error
}

// MemorySnapshot is one measurement of the host's unified memory state.
// Allocatable is the static share of physical memory this host offers to
// Idleloom. Available is how much of that share the host could actually give a
// new workload right now, after accounting for what every other process holds.
type MemorySnapshot struct {
	Total         resource.Quantity
	Allocatable   resource.Quantity
	Available     resource.Quantity
	PressureLevel int
	SwapUsedBytes int64
	MeasuredAt    time.Time
}

// macOS kern.memorystatus_vm_pressure_level values.
const (
	memoryPressureNormal   = 1
	memoryPressureWarning  = 2
	memoryPressureCritical = 4
)

// Swap below this size is treated as background noise rather than pressure.
const swapUsageGraceBytes = int64(256 << 20)

type DarwinPlatform struct{}

func (DarwinPlatform) KrunkitRunning(ctx context.Context) (bool, error) {
	err := exec.CommandContext(ctx, "pgrep", "-x", "krunkit").Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect krunkit process state: %w", err)
}

func (DarwinPlatform) AllocatableMemory(ctx context.Context) (resource.Quantity, error) {
	total, err := totalMemoryBytes(ctx)
	if err != nil {
		return resource.Quantity{}, err
	}
	return *resource.NewQuantity(total*7/10, resource.BinarySI), nil
}

// MemorySnapshot measures how much unified memory the host could actually hand
// to a new workload. The page accounting comes from vm_stat; the pressure level
// and swap usage narrow it, because a host under memory pressure reclaims the
// same pages this snapshot would otherwise count as available.
func (DarwinPlatform) MemorySnapshot(ctx context.Context) (MemorySnapshot, error) {
	total, err := totalMemoryBytes(ctx)
	if err != nil {
		return MemorySnapshot{}, err
	}
	vmStatOutput, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return MemorySnapshot{}, fmt.Errorf("read vm_stat: %w", err)
	}
	pageSize, pages, err := parseVMStat(vmStatOutput)
	if err != nil {
		return MemorySnapshot{}, err
	}
	pressure := memoryPressureNormal
	if output, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level").Output(); err == nil {
		if level, err := strconv.Atoi(strings.TrimSpace(string(output))); err == nil && level > 0 {
			pressure = level
		}
	}
	var swapUsed int64
	if output, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.swapusage").Output(); err == nil {
		if parsed, err := parseSwapUsage(string(output)); err == nil {
			swapUsed = parsed
		}
	}
	available := availableBytes(pageSize, pages, pressure, swapUsed)
	return MemorySnapshot{
		Total:         *resource.NewQuantity(total, resource.BinarySI),
		Allocatable:   *resource.NewQuantity(total*7/10, resource.BinarySI),
		Available:     *resource.NewQuantity(available, resource.BinarySI),
		PressureLevel: pressure,
		SwapUsedBytes: swapUsed,
		MeasuredAt:    time.Now(),
	}, nil
}

func totalMemoryBytes(ctx context.Context) (int64, error) {
	output, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, fmt.Errorf("read unified memory capacity: %w", err)
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || bytes <= 0 {
		return 0, fmt.Errorf("invalid unified memory capacity %q", strings.TrimSpace(string(output)))
	}
	return bytes, nil
}

// availableBytes turns raw page counts into a conservative available figure.
// Under pressure the kernel is already reclaiming, so inactive and speculative
// pages stop counting; under critical pressure nothing does. Swap use beyond a
// small grace also subtracts, because a swapping host pays for new allocations
// with page-outs rather than with free memory.
func availableBytes(pageSize int64, pages map[string]int64, pressure int, swapUsed int64) int64 {
	var pageCount int64
	switch {
	case pressure >= memoryPressureCritical:
		pageCount = 0
	case pressure >= memoryPressureWarning:
		pageCount = pages["Pages free"] + pages["Pages purgeable"]
	default:
		pageCount = pages["Pages free"] + pages["Pages inactive"] + pages["Pages purgeable"] + pages["Pages speculative"]
	}
	available := pageCount * pageSize
	if penalty := swapUsed - swapUsageGraceBytes; penalty > 0 {
		available -= penalty
	}
	if available < 0 {
		available = 0
	}
	return available
}

// parseVMStat reads the page size and per-category page counts from vm_stat
// output, for example:
//
//	Mach Virtual Memory Statistics: (page size of 16384 bytes)
//	Pages free:                              110082.
func parseVMStat(output []byte) (int64, map[string]int64, error) {
	lines := strings.Split(string(output), "\n")
	if len(lines) == 0 {
		return 0, nil, fmt.Errorf("empty vm_stat output")
	}
	const pageSizeMarker = "page size of "
	header := lines[0]
	start := strings.Index(header, pageSizeMarker)
	if start < 0 {
		return 0, nil, fmt.Errorf("vm_stat header missing page size: %q", header)
	}
	sizeText := header[start+len(pageSizeMarker):]
	if end := strings.IndexByte(sizeText, ' '); end > 0 {
		sizeText = sizeText[:end]
	}
	pageSize, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || pageSize <= 0 {
		return 0, nil, fmt.Errorf("invalid vm_stat page size %q", sizeText)
	}
	pages := make(map[string]int64, 8)
	for _, line := range lines[1:] {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		count, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil || count < 0 {
			continue
		}
		pages[strings.TrimSpace(key)] = count
	}
	if _, ok := pages["Pages free"]; !ok {
		return 0, nil, fmt.Errorf("vm_stat output missing free page count")
	}
	return pageSize, pages, nil
}

// parseSwapUsage extracts the used bytes from vm.swapusage output, for example:
//
//	total = 2048.00M  used = 1198.75M  free = 849.25M  (encrypted)
func parseSwapUsage(output string) (int64, error) {
	const usedMarker = "used = "
	start := strings.Index(output, usedMarker)
	if start < 0 {
		return 0, fmt.Errorf("vm.swapusage output missing used figure: %q", strings.TrimSpace(output))
	}
	text := output[start+len(usedMarker):]
	if end := strings.IndexByte(text, ' '); end > 0 {
		text = text[:end]
	}
	if text == "" {
		return 0, fmt.Errorf("empty vm.swapusage used figure")
	}
	unit := text[len(text)-1]
	var scale float64
	switch unit {
	case 'K':
		scale = 1 << 10
	case 'M':
		scale = 1 << 20
	case 'G':
		scale = 1 << 30
	default:
		return 0, fmt.Errorf("unrecognised vm.swapusage unit in %q", text)
	}
	value, err := strconv.ParseFloat(text[:len(text)-1], 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid vm.swapusage used figure %q", text)
	}
	return int64(value * scale), nil
}

func (DarwinPlatform) ProcessStartToken(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("read process start token: %w", err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("process %d has no start token", pid)
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(pid) + "\x00" + value))
	return hex.EncodeToString(sum[:]), nil
}

func (DarwinPlatform) ProcessAlive(pid int) (bool, error) {
	err := unix.Kill(pid, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return false, err
}

func (DarwinPlatform) FindRunnerPIDs(ctx context.Context, runner, nonce string) ([]int, error) {
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, runner) || !strings.Contains(line, nonce) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (DarwinPlatform) KillProcessGroupAndWait(ctx context.Context, pid int) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := unix.Kill(-pid, 0); errors.Is(err, unix.ESRCH) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("process group %d did not exit", pid)
		case <-ticker.C:
		}
	}
}
