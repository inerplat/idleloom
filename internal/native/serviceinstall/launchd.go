package serviceinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	nativewirekube "github.com/inerplat/idleloom/internal/native/wirekube"
)

const receiptFileName = "services.json"

const rootReceiptFileName = "service-owner.json"

type Config struct {
	HostID                     string
	StateDirectory             string
	RuntimeRoot                string
	Namespace                  string
	AgentID                    string
	LinkMode                   string
	ControllerKubeconfig       string
	AgentKubeconfig            string
	LinkKubeconfig             string
	ProjectionKubeconfig       string
	BinaryDirectory            string
	PublicBinary               []byte
	KubeletClientCommonNames   string
	KubeletClientOrganizations string
}

type Receipt struct {
	Version    int      `json:"version"`
	HostID     string   `json:"hostID"`
	UserLabels []string `json:"userLabels"`
	RootLabel  string   `json:"rootLabel,omitempty"`
	RootPhase  string   `json:"rootPhase,omitempty"`
}

type service struct {
	label       string
	program     string
	arguments   []string
	environment []string
	stdout      string
	stderr      string
}

type rootReceipt struct {
	Version        int    `json:"version"`
	HostID         string `json:"hostID"`
	Label          string `json:"label"`
	StateDirectory string `json:"stateDirectory"`
}

func Install(ctx context.Context, config Config) (Receipt, error) {
	if runtime.GOOS != "darwin" {
		return Receipt{}, fmt.Errorf("native services require macOS launchd")
	}
	if config.HostID == "" || config.StateDirectory == "" || config.Namespace == "" || config.AgentID == "" || config.ControllerKubeconfig == "" || config.AgentKubeconfig == "" {
		return Receipt{}, fmt.Errorf("host, state, namespace, agent, and kubeconfig fields are required")
	}
	if config.BinaryDirectory == "" {
		executable, err := os.Executable()
		if err != nil {
			return Receipt{}, err
		}
		config.BinaryDirectory = filepath.Dir(executable)
	}
	if config.RuntimeRoot == "" {
		config.RuntimeRoot = filepath.Join(string(filepath.Separator), "var", "tmp", "idleloom")
	}
	if _, err := os.Stat(filepath.Join(config.StateDirectory, receiptFileName)); err == nil {
		return Receipt{}, fmt.Errorf("native services are already installed; delete the joined host before joining again")
	} else if !os.IsNotExist(err) {
		return Receipt{}, err
	}
	logsDirectory := filepath.Join(config.StateDirectory, "logs")
	binDirectory := filepath.Join(config.StateDirectory, "bin")
	for _, directory := range []string{logsDirectory, binDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return Receipt{}, err
		}
	}

	copyBinary := func(name string) (string, error) {
		source := filepath.Join(config.BinaryDirectory, name)
		destination := filepath.Join(binDirectory, name)
		if err := copyFile(source, destination, 0o700); err != nil {
			return "", fmt.Errorf("install %s: %w", name, err)
		}
		return destination, nil
	}
	controller, err := copyBinary("idleloom-controller")
	if err != nil {
		return Receipt{}, err
	}
	agent, err := copyBinary("idleloom-agent")
	if err != nil {
		return Receipt{}, err
	}
	hostSuffix := labelSuffix(config.HostID)
	home, err := os.UserHomeDir()
	if err != nil {
		return Receipt{}, err
	}
	userEnvironment := []string{
		"HOME=" + home,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=" + os.TempDir(),
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	services := []service{
		{
			label: "io.idleloom.controller." + hostSuffix, program: controller,
			arguments:   []string{"--kubeconfig", config.ControllerKubeconfig, "--interval", "2s"},
			environment: userEnvironment,
			stdout:      filepath.Join(logsDirectory, "controller.log"), stderr: filepath.Join(logsDirectory, "controller.log"),
		},
		{
			label: "io.idleloom.agent." + hostSuffix, program: agent,
			arguments: []string{
				"--kubeconfig", config.AgentKubeconfig, "--namespace", config.Namespace,
				"--agent-id", config.AgentID, "--root", config.RuntimeRoot,
				"--state-dir", config.StateDirectory, "--link", config.LinkMode,
				"--kubelet-client-cn", config.KubeletClientCommonNames,
				"--kubelet-client-organization", config.KubeletClientOrganizations,
			},
			environment: userEnvironment,
			stdout:      filepath.Join(logsDirectory, "agent.log"), stderr: filepath.Join(logsDirectory, "agent.log"),
		},
	}
	if config.ProjectionKubeconfig != "" {
		projection, copyErr := copyBinary("idleloom-projection")
		if copyErr != nil {
			return Receipt{}, copyErr
		}
		services = append(services, service{
			label: "io.idleloom.projection." + hostSuffix, program: projection,
			arguments: []string{
				"--kubeconfig", config.ProjectionKubeconfig, "--state-dir", config.StateDirectory,
				"--enable-kubernetes-projection", "--interval", "2s",
			},
			environment: userEnvironment,
			stdout:      filepath.Join(logsDirectory, "projection.log"), stderr: filepath.Join(logsDirectory, "projection.log"),
		})
	}

	receipt := Receipt{Version: 1, HostID: config.HostID}
	if config.LinkMode == "wirekube" {
		rootLabel := "io.idleloom.link." + hostSuffix
		rootHelper, rootPlist, rootStateDirectory, err := rootArtifacts(rootLabel)
		if err != nil {
			return Receipt{}, err
		}
		state, err := nativewirekube.ReadState(config.StateDirectory)
		if err != nil {
			return Receipt{}, fmt.Errorf("validate connected-leaf state: %w", err)
		}
		if len(config.PublicBinary) == 0 {
			return Receipt{}, fmt.Errorf("captured public native binary is required for the privileged link")
		}
		publicBinaryData, err := readRegularFile(filepath.Join(config.BinaryDirectory, "idlectl"), 256<<20)
		if err != nil {
			return Receipt{}, fmt.Errorf("read public native binary: %w", err)
		}
		companionData, err := readRegularFile(filepath.Join(config.BinaryDirectory, "idleloom-link"), 256<<20)
		if err != nil {
			return Receipt{}, fmt.Errorf("read link companion: %w", err)
		}
		if !sameBinary(config.PublicBinary, publicBinaryData) || !sameBinary(config.PublicBinary, companionData) {
			return Receipt{}, fmt.Errorf("native bundle binaries do not match the running public binary")
		}
		rootState := state
		rootState.LinkKubeconfig = nativewirekube.LinkKubeconfigPath(rootStateDirectory)
		stateData, err := json.MarshalIndent(rootState, "", "  ")
		if err != nil {
			return Receipt{}, err
		}
		rootService := service{
			label: rootLabel, program: rootHelper,
			arguments: []string{
				"--state-dir", rootStateDirectory,
				"--kubeconfig", nativewirekube.LinkKubeconfigPath(rootStateDirectory),
				"--enable-wirekube-connected-leaf",
			},
			environment: []string{"HOME=/var/root", "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=/tmp", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"},
			stdout:      filepath.Join(rootStateDirectory, "link.log"), stderr: filepath.Join(rootStateDirectory, "link.log"),
		}
		plistData := launchdPlist(rootService)
		canonicalStateDirectory, err := canonicalPath(config.StateDirectory)
		if err != nil {
			return Receipt{}, err
		}
		rootReceiptData, err := json.MarshalIndent(rootReceipt{
			Version: 1, HostID: config.HostID, Label: rootLabel, StateDirectory: canonicalStateDirectory,
		}, "", "  ")
		if err != nil {
			return Receipt{}, err
		}
		receipt.RootLabel, receipt.RootPhase = rootLabel, "planned"
		if err := writeReceipt(config.StateDirectory, receipt); err != nil {
			return Receipt{}, err
		}
		for _, path := range []string{rootStateDirectory, rootHelper, rootPlist} {
			if err := sudo(ctx, "/bin/test", "!", "-e", path); err != nil {
				_ = os.Remove(filepath.Join(config.StateDirectory, receiptFileName))
				return Receipt{}, fmt.Errorf("privileged link artifact already exists or cannot be inspected at %s: %w", path, err)
			}
		}
		if err := sudo(ctx, "install", "-d", "-o", "root", "-g", "wheel", "-m", "0700", rootStateDirectory); err != nil {
			_ = os.Remove(filepath.Join(config.StateDirectory, receiptFileName))
			return Receipt{}, fmt.Errorf("create privileged link state: %w", err)
		}
		if err := sudoWriteFile(ctx, filepath.Join(rootStateDirectory, rootReceiptFileName), append(rootReceiptData, '\n'), 0o600); err != nil {
			_ = sudo(ctx, "rm", "-rf", rootStateDirectory)
			_ = os.Remove(filepath.Join(config.StateDirectory, receiptFileName))
			return Receipt{}, fmt.Errorf("install privileged service ownership: %w", err)
		}
		receipt.RootPhase = "owned"
		if err := writeReceipt(config.StateDirectory, receipt); err != nil {
			_ = removeRoot(ctx, config.StateDirectory, receipt)
			return Receipt{}, err
		}
		if err := sudoWriteFile(ctx, nativewirekube.StatePath(rootStateDirectory), append(stateData, '\n'), 0o600); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("install privileged link state: %w", err)
		}
		linkKubeconfig, err := readRegularFile(config.LinkKubeconfig, 1<<20)
		if err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("read restricted WireKube peer kubeconfig: %w", err)
		}
		if err := sudoWriteFile(ctx, nativewirekube.LinkKubeconfigPath(rootStateDirectory), linkKubeconfig, 0o600); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("install restricted WireKube peer kubeconfig: %w", err)
		}
		if err := sudoWriteFile(ctx, rootHelper, config.PublicBinary, 0o755); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("install privileged link helper: %w", err)
		}
		if err := sudoWriteFile(ctx, rootPlist, plistData, 0o644); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("install link LaunchDaemon: %w", err)
		}
		_ = sudoQuiet(ctx, "launchctl", "bootout", "system/"+rootLabel)
		time.Sleep(250 * time.Millisecond)
		if err := retry(10, 250*time.Millisecond, func() error { return sudo(ctx, "launchctl", "bootstrap", "system", rootPlist) }); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("bootstrap link LaunchDaemon: %w", err)
		}
	}
	if err := writeReceipt(config.StateDirectory, receipt); err != nil {
		_ = removeRoot(ctx, config.StateDirectory, receipt)
		return Receipt{}, err
	}

	uid := strconv.Itoa(os.Getuid())
	launchAgents, err := userLaunchAgentsDirectory()
	if err != nil {
		_ = removeRoot(ctx, config.StateDirectory, receipt)
		return Receipt{}, err
	}
	if err := os.MkdirAll(launchAgents, 0o755); err != nil {
		_ = removeRoot(ctx, config.StateDirectory, receipt)
		return Receipt{}, err
	}
	for _, item := range services {
		plistPath := filepath.Join(launchAgents, item.label+".plist")
		receipt.UserLabels = append(receipt.UserLabels, item.label)
		if err := writeReceipt(config.StateDirectory, receipt); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, err
		}
		if err := os.WriteFile(plistPath, launchdPlist(item), 0o644); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, err
		}
		_ = command(ctx, "/bin/launchctl", "bootout", "gui/"+uid+"/"+item.label)
		time.Sleep(100 * time.Millisecond)
		if err := retry(20, 100*time.Millisecond, func() error { return command(ctx, "/bin/launchctl", "bootstrap", "gui/"+uid, plistPath) }); err != nil {
			_ = Remove(ctx, config.StateDirectory)
			return Receipt{}, fmt.Errorf("bootstrap %s: %w", item.label, err)
		}
	}
	if err := writeReceipt(config.StateDirectory, receipt); err != nil {
		_ = Remove(ctx, config.StateDirectory)
		return Receipt{}, err
	}
	return receipt, nil
}

func Remove(ctx context.Context, stateDirectory string) error {
	data, err := os.ReadFile(filepath.Join(stateDirectory, receiptFileName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if err := validateReceipt(receipt); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	launchAgents, err := userLaunchAgentsDirectory()
	if err != nil {
		return err
	}
	var errs []error
	for _, label := range receipt.UserLabels {
		if err := command(ctx, "/bin/launchctl", "bootout", "gui/"+uid+"/"+label); err != nil && !isNotLoaded(err) {
			errs = append(errs, err)
		}
		if err := os.Remove(filepath.Join(launchAgents, label+".plist")); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, removeRoot(ctx, stateDirectory, receipt))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return os.Remove(filepath.Join(stateDirectory, receiptFileName))
}

func launchdPlist(item service) []byte {
	var output strings.Builder
	output.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	output.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	output.WriteString("<plist version=\"1.0\"><dict>\n")
	writeKeyString(&output, "Label", item.label)
	output.WriteString("<key>ProgramArguments</key><array>")
	writeString(&output, "/usr/bin/env")
	writeString(&output, "-i")
	for _, variable := range item.environment {
		writeString(&output, variable)
	}
	writeString(&output, item.program)
	for _, argument := range item.arguments {
		writeString(&output, argument)
	}
	output.WriteString("</array>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n")
	output.WriteString("<key>ProcessType</key><string>Background</string>\n<key>ThrottleInterval</key><integer>5</integer>\n")
	writeKeyString(&output, "StandardOutPath", item.stdout)
	writeKeyString(&output, "StandardErrorPath", item.stderr)
	output.WriteString("</dict></plist>\n")
	return []byte(output.String())
}

func writeKeyString(output *strings.Builder, key, value string) {
	output.WriteString("<key>")
	escapeXML(output, key)
	output.WriteString("</key>")
	writeString(output, value)
	output.WriteByte('\n')
}

func writeString(output *strings.Builder, value string) {
	output.WriteString("<string>")
	escapeXML(output, value)
	output.WriteString("</string>")
}

func escapeXML(output io.Writer, value string) {
	_ = xml.EscapeText(output, []byte(value))
}

func writeReceipt(directory string, receipt Receipt) error {
	if err := validateReceipt(receipt); err != nil {
		return err
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".services-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
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
	return os.Rename(temporaryName, filepath.Join(directory, receiptFileName))
}

// HasReceipt reports whether launchd services have been installed for state.
func HasReceipt(stateDirectory string) (bool, error) {
	_, err := os.Stat(filepath.Join(stateDirectory, receiptFileName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func removeRoot(ctx context.Context, stateDirectory string, receipt Receipt) error {
	if receipt.RootLabel == "" {
		return nil
	}
	rootHelper, rootPlist, rootStateDirectory, err := rootArtifacts(receipt.RootLabel)
	if err != nil {
		return err
	}
	if err := verifyRootReceipt(ctx, stateDirectory, receipt, rootStateDirectory); err != nil {
		if receipt.RootPhase != "planned" {
			return err
		}
		if cleanErr := cleanUnownedRootPlan(ctx, rootHelper, rootPlist, rootStateDirectory); cleanErr != nil {
			return errors.Join(err, cleanErr)
		}
		return nil
	}
	var errs []error
	if err := sudoQuiet(ctx, "launchctl", "bootout", "system/"+receipt.RootLabel); err != nil && !isNotLoaded(err) {
		errs = append(errs, err)
	}
	if err := sudo(ctx, "rm", "-f", rootPlist, rootHelper); err != nil {
		errs = append(errs, err)
	}
	if err := sudo(ctx, "rm", "-rf", rootStateDirectory); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func cleanUnownedRootPlan(ctx context.Context, rootHelper, rootPlist, rootStateDirectory string) error {
	for _, path := range []string{rootHelper, rootPlist} {
		if err := sudo(ctx, "/bin/test", "!", "-e", path); err != nil {
			return fmt.Errorf("refusing to clean unowned privileged artifact %s: %w", path, err)
		}
	}
	if err := sudo(ctx, "/bin/test", "!", "-e", rootStateDirectory); err == nil {
		return nil
	}
	entries, err := sudoListDirectory(ctx, rootStateDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validOwnershipTemporaryName(entry) {
			return fmt.Errorf("refusing to clean unexpected unowned privileged state entry %q", entry)
		}
		if err := sudo(ctx, "rm", "-f", filepath.Join(rootStateDirectory, entry)); err != nil {
			return err
		}
	}
	if err := sudo(ctx, "rmdir", rootStateDirectory); err != nil {
		return fmt.Errorf("refusing to clean non-empty unowned privileged state %s: %w", rootStateDirectory, err)
	}
	return nil
}

func validOwnershipTemporaryName(name string) bool {
	prefix := rootReceiptFileName + ".idleloom-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".new") {
		return false
	}
	processID := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".new")
	if processID == "" {
		return false
	}
	for _, value := range processID {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func verifyRootReceipt(ctx context.Context, stateDirectory string, receipt Receipt, rootStateDirectory string) error {
	data, err := sudoReadFile(ctx, filepath.Join(rootStateDirectory, rootReceiptFileName))
	if err != nil {
		return fmt.Errorf("read privileged service ownership: %w", err)
	}
	var owner rootReceipt
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&owner); err != nil {
		return fmt.Errorf("decode privileged service ownership: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("decode privileged service ownership: trailing data")
	}
	canonicalStateDirectory, err := canonicalPath(stateDirectory)
	if err != nil {
		return err
	}
	if err := validateRootOwnership(owner, receipt, canonicalStateDirectory); err != nil {
		return err
	}
	return nil
}

func validateRootOwnership(owner rootReceipt, receipt Receipt, canonicalStateDirectory string) error {
	if owner.Version != 1 || owner.HostID != receipt.HostID || owner.Label != receipt.RootLabel || owner.StateDirectory != canonicalStateDirectory {
		return fmt.Errorf("privileged service ownership does not match local enrollment")
	}
	return nil
}

func rootArtifacts(label string) (helper, plist, stateDirectory string, err error) {
	const prefix = "io.idleloom.link."
	suffix := strings.TrimPrefix(label, prefix)
	if suffix == label || suffix == "" {
		return "", "", "", fmt.Errorf("invalid link service label %q", label)
	}
	for _, value := range suffix {
		if value < 'a' || value > 'z' {
			if value < '0' || value > '9' {
				if value != '-' {
					return "", "", "", fmt.Errorf("invalid link service label %q", label)
				}
			}
		}
	}
	helper = filepath.Join("/Library/PrivilegedHelperTools", label)
	plist = filepath.Join("/Library/LaunchDaemons", label+".plist")
	stateDirectory = filepath.Join("/Library/Application Support/Idleloom/Native", label)
	return helper, plist, stateDirectory, nil
}

func validateReceipt(receipt Receipt) error {
	if receipt.Version != 1 {
		return fmt.Errorf("unsupported native service receipt version %d", receipt.Version)
	}
	hostSuffix := labelSuffix(receipt.HostID)
	if hostSuffix == "" {
		return fmt.Errorf("native service receipt host ID is invalid")
	}
	for _, label := range receipt.UserLabels {
		if !validUserServiceLabel(label, hostSuffix) {
			return fmt.Errorf("invalid user service label %q", label)
		}
	}
	if receipt.RootLabel != "" {
		if receipt.RootPhase != "planned" && receipt.RootPhase != "owned" {
			return fmt.Errorf("link service phase is invalid")
		}
		if receipt.RootLabel != "io.idleloom.link."+hostSuffix {
			return fmt.Errorf("link service label does not match host %q", receipt.HostID)
		}
		if _, _, _, err := rootArtifacts(receipt.RootLabel); err != nil {
			return err
		}
	}
	if receipt.RootLabel == "" && receipt.RootPhase != "" {
		return fmt.Errorf("link service phase has no service label")
	}
	return nil
}

func validUserServiceLabel(label, hostSuffix string) bool {
	for _, prefix := range []string{
		"io.idleloom.controller.",
		"io.idleloom.agent.",
		"io.idleloom.projection.",
	} {
		if label == prefix+hostSuffix {
			return true
		}
	}
	return false
}

func readRegularFile(path string, maximumBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	if info.Size() <= 0 || info.Size() > maximumBytes {
		return nil, fmt.Errorf("%s has invalid size %d", path, info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumBytes {
		return nil, fmt.Errorf("%s grew beyond the maximum size", path)
	}
	return data, nil
}

func sameBinary(left, right []byte) bool {
	leftDigest := sha256.Sum256(left)
	rightDigest := sha256.Sum256(right)
	return leftDigest == rightDigest
}

// CaptureCurrentBinary reads the running CLI before any administrator prompt.
func CaptureCurrentBinary() ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	data, err := readRegularFile(executable, 256<<20)
	if err != nil {
		return nil, err
	}
	fileHash, err := machoCodeHash(data)
	if err != nil {
		return nil, fmt.Errorf("read executable code signature: %w", err)
	}
	runningHash, err := runningCodeHash()
	if err != nil {
		return nil, fmt.Errorf("read running code identity: %w", err)
	}
	if !bytes.Equal(fileHash, runningHash) {
		return nil, fmt.Errorf("public native binary changed after process start")
	}
	return data, nil
}

func machoCodeHash(data []byte) ([]byte, error) {
	const (
		machHeader64Size   = 32
		loadCodeSignature  = 0x1d
		superBlobMagic     = 0xfade0cc0
		codeDirectorySlot  = 0
		codeDirectoryMagic = 0xfade0c02
	)
	if len(data) < machHeader64Size || binary.LittleEndian.Uint32(data[:4]) != 0xfeedfacf {
		return nil, fmt.Errorf("running executable is not a thin 64-bit Mach-O")
	}
	numberOfCommands := int(binary.LittleEndian.Uint32(data[16:20]))
	offset := machHeader64Size
	var signature []byte
	for index := 0; index < numberOfCommands; index++ {
		if offset+8 > len(data) {
			return nil, fmt.Errorf("Mach-O load commands are truncated")
		}
		command := binary.LittleEndian.Uint32(data[offset : offset+4])
		commandSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if commandSize < 8 || offset+commandSize > len(data) {
			return nil, fmt.Errorf("Mach-O load command is invalid")
		}
		if command == loadCodeSignature {
			if commandSize < 16 {
				return nil, fmt.Errorf("Mach-O code signature command is truncated")
			}
			dataOffset := int(binary.LittleEndian.Uint32(data[offset+8 : offset+12]))
			dataSize := int(binary.LittleEndian.Uint32(data[offset+12 : offset+16]))
			if dataOffset < 0 || dataSize <= 0 || dataOffset+dataSize > len(data) {
				return nil, fmt.Errorf("Mach-O code signature range is invalid")
			}
			signature = data[dataOffset : dataOffset+dataSize]
			break
		}
		offset += commandSize
	}
	if len(signature) < 12 || binary.BigEndian.Uint32(signature[:4]) != superBlobMagic {
		return nil, fmt.Errorf("Mach-O embedded signature is missing")
	}
	signatureLength := int(binary.BigEndian.Uint32(signature[4:8]))
	count := int(binary.BigEndian.Uint32(signature[8:12]))
	if signatureLength > len(signature) || count < 1 || 12+count*8 > signatureLength {
		return nil, fmt.Errorf("Mach-O embedded signature is invalid")
	}
	for index := 0; index < count; index++ {
		entry := 12 + index*8
		if binary.BigEndian.Uint32(signature[entry:entry+4]) != codeDirectorySlot {
			continue
		}
		blobOffset := int(binary.BigEndian.Uint32(signature[entry+4 : entry+8]))
		if blobOffset < 0 || blobOffset+8 > signatureLength {
			return nil, fmt.Errorf("Mach-O code directory offset is invalid")
		}
		codeDirectory := signature[blobOffset:signatureLength]
		if binary.BigEndian.Uint32(codeDirectory[:4]) != codeDirectoryMagic {
			return nil, fmt.Errorf("Mach-O code directory is invalid")
		}
		length := int(binary.BigEndian.Uint32(codeDirectory[4:8]))
		if length < 8 || length > len(codeDirectory) {
			return nil, fmt.Errorf("Mach-O code directory length is invalid")
		}
		digest := sha256.Sum256(codeDirectory[:length])
		return append([]byte(nil), digest[:20]...), nil
	}
	return nil, fmt.Errorf("Mach-O primary code directory is missing")
}

func sudoWriteFile(ctx context.Context, destination string, data []byte, mode os.FileMode) error {
	temporary := fmt.Sprintf("%s.idleloom-%d.new", destination, os.Getpid())
	_ = sudo(ctx, "rm", "-f", temporary)
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "/usr/bin/tee", temporary)
	command.Stdin = bytes.NewReader(data)
	command.Stdout = io.Discard
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("sudo tee %s: %w", temporary, err)
	}
	cleanup := func() { _ = sudo(context.Background(), "rm", "-f", temporary) }
	if err := sudo(ctx, "chown", "root:wheel", temporary); err != nil {
		cleanup()
		return err
	}
	if err := sudo(ctx, "chmod", fmt.Sprintf("%04o", mode.Perm()), temporary); err != nil {
		cleanup()
		return err
	}
	if err := sudo(ctx, "mv", "-f", temporary, destination); err != nil {
		cleanup()
		return err
	}
	return nil
}

func sudoReadFile(ctx context.Context, path string) ([]byte, error) {
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "/bin/cat", path)
	command.Stdin = os.Stdin
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("sudo cat %s: %w", path, err)
	}
	if output.Len() > 64<<10 {
		return nil, fmt.Errorf("privileged service ownership is too large")
	}
	return output.Bytes(), nil
}

func sudoListDirectory(ctx context.Context, path string) ([]string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "/bin/ls", "-A1", path)
	command.Stdin = os.Stdin
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("sudo ls %s: %w", path, err)
	}
	if output.Len() > 64<<10 {
		return nil, fmt.Errorf("privileged state directory listing is too large")
	}
	var entries []string
	for _, entry := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func command(ctx context.Context, name string, arguments ...string) error {
	cmd := exec.CommandContext(ctx, name, arguments...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func sudo(ctx context.Context, arguments ...string) error {
	resolved, err := resolveSudoArguments(arguments)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", resolved...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %s: %w", strings.Join(resolved, " "), err)
	}
	return nil
}

func sudoQuiet(ctx context.Context, arguments ...string) error {
	resolved, err := resolveSudoArguments(arguments)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", resolved...)
	var output bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %s: %w: %s", strings.Join(resolved, " "), err, strings.TrimSpace(output.String()))
	}
	return nil
}

func resolveSudoArguments(arguments []string) ([]string, error) {
	if len(arguments) == 0 {
		return nil, fmt.Errorf("sudo command is required")
	}
	if filepath.IsAbs(arguments[0]) {
		return arguments, nil
	}
	path, ok := map[string]string{
		"install":   "/usr/bin/install",
		"launchctl": "/bin/launchctl",
		"rm":        "/bin/rm",
		"chown":     "/usr/sbin/chown",
		"chmod":     "/bin/chmod",
		"mv":        "/bin/mv",
		"rmdir":     "/bin/rmdir",
	}[arguments[0]]
	if !ok {
		return nil, fmt.Errorf("unsupported privileged command %q", arguments[0])
	}
	return append([]string{path}, arguments[1:]...), nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".native-service-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, destination)
}

func userLaunchAgentsDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func labelSuffix(hostID string) string {
	var output strings.Builder
	for _, value := range strings.ToLower(hostID) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
			output.WriteRune(value)
		} else {
			output.WriteByte('-')
		}
	}
	return strings.Trim(output.String(), "-")
}

func isNotLoaded(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "Could not find specified service") || strings.Contains(err.Error(), "No such process"))
}

func retry(attempts int, delay time.Duration, operation func() error) error {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if attempt+1 < attempts {
			time.Sleep(delay)
		}
	}
	return err
}
