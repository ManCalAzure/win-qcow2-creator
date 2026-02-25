package packager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

type Config struct {
	Backend               string
	Profile               string
	VMName                string
	OS                    string
	WindowsISO            string
	DriversDir            string
	CloudbaseInitMSI      string
	OutputImage           string
	WorkDir               string
	DiskSize              string
	CPUs                  int
	MemoryMB              int
	Headless              bool
	VNCListen             string
	QemuSystemBin         string
	QemuImgBin            string
	SWTPMBin              string
	QemuAccel             string
	QemuCPU               string
	OVMFCode              string
	OVMFVarsTemplate      string
	WindowsEditionIndex   int
	AdminPassword         string
	AdminUsername         string
	EnableRDP             bool
	OptimizeForSize       bool
	FastMode              bool
	SkipFinalCompact      bool
	CompressFinal         bool
	QemuImgConvertThreads int
	LogWriter             io.Writer
	Interactive           bool
}

type buildContext struct {
	cfg             Config
	vmName          string
	buildDir        string
	diskPath        string
	finalOutputPath string
	driverISORoot   string
	driverISOPath   string
	answerRoot      string
	answerISOPath   string
	ovmfVarsPath    string
	tpmDir          string
	tpmSock         string
	logWriter       io.Writer
	osProfile       osProfile
}

type osProfile struct {
	Name          string
	TemplatePath  string
	VirtioFlavor  string
	EnableTPM     bool
	DriverFolders []string
}

type templateData struct {
	AdminPassword       string
	WindowsEditionIndex int
	VirtioFlavor        string
	AdminUsername       string
	EnableRDP           bool
	OptimizeForSize     bool
	GoldenProfile       bool
}

func Run(ctx context.Context, cfg Config) error {
	cfg.applyProfileDefaults()
	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "Administrator"
	}
	if cfg.LogWriter == nil {
		cfg.LogWriter = os.Stdout
	}
	if cfg.FastMode && cfg.QemuImgConvertThreads <= 0 {
		cfg.QemuImgConvertThreads = max(1, cfg.CPUs)
	}
	cfg.QemuSystemBin = resolveExecutable(cfg.QemuSystemBin, []string{"/usr/bin/qemu-system-x86_64", "/usr/local/bin/qemu-system-x86_64"})
	cfg.QemuImgBin = resolveExecutable(cfg.QemuImgBin, []string{"/usr/bin/qemu-img", "/usr/local/bin/qemu-img"})
	cfg.SWTPMBin = resolveExecutable(cfg.SWTPMBin, []string{"/usr/bin/swtpm", "/usr/local/bin/swtpm"})

	if err := validateConfig(cfg); err != nil {
		return err
	}

	profile, err := getOSProfile(cfg.OS)
	if err != nil {
		return err
	}

	stamp := time.Now().Format("20060102-150405")
	buildDir := filepath.Join(cfg.WorkDir, fmt.Sprintf("%s-%s", cfg.OS, stamp))
	vmName := cfg.VMName
	if strings.TrimSpace(vmName) == "" {
		vmName = fmt.Sprintf("%s-build-%s", cfg.OS, stamp)
	}

	bc := buildContext{
		cfg:             cfg,
		vmName:          vmName,
		buildDir:        buildDir,
		diskPath:        filepath.Join(buildDir, "disk.raw.qcow2"),
		finalOutputPath: cfg.OutputImage,
		driverISORoot:   filepath.Join(buildDir, "driver-bundle"),
		driverISOPath:   filepath.Join(buildDir, "virtio-bundle.iso"),
		answerRoot:      filepath.Join(buildDir, "answer-media"),
		answerISOPath:   filepath.Join(buildDir, "answer.iso"),
		ovmfVarsPath:    filepath.Join(buildDir, "OVMF_VARS.fd"),
		tpmDir:          filepath.Join(buildDir, "swtpm"),
		tpmSock:         filepath.Join(buildDir, "swtpm", "swtpm-sock"),
		logWriter:       cfg.LogWriter,
		osProfile:       profile,
	}

	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}

	var prepWG sync.WaitGroup
	prepWG.Add(2)
	errCh := make(chan error, 2)
	go func() {
		defer prepWG.Done()
		if err := prepareDriverMedia(ctx, bc); err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer prepWG.Done()
		if err := prepareAnswerMedia(bc); err != nil {
			errCh <- err
		}
	}()
	prepWG.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}

	if err := buildISO(ctx, bc.logWriter, bc.answerRoot, bc.answerISOPath); err != nil {
		return fmt.Errorf("build answer ISO: %w", err)
	}

	if err := copyFile(cfg.OVMFVarsTemplate, bc.ovmfVarsPath); err != nil {
		return fmt.Errorf("prepare OVMF vars file: %w", err)
	}

	if err := runCmd(ctx, bc.logWriter, cfg.QemuImgBin, "create", "-f", "qcow2", "-o", "cluster_size=65536,lazy_refcounts=on,preallocation=metadata", bc.diskPath, cfg.DiskSize); err != nil {
		return fmt.Errorf("create qcow2: %w", err)
	}

	if err := runInstallVM(ctx, bc); err != nil {
		return err
	}

	if err := compactImage(ctx, bc); err != nil {
		return err
	}

	return nil
}

func (cfg *Config) applyProfileDefaults() {
	profile := strings.ToLower(strings.TrimSpace(cfg.Profile))
	if profile == "" || profile == "default" {
		return
	}
	if profile != "golden" {
		return
	}
	cfg.EnableRDP = true
	cfg.OptimizeForSize = true
	if cfg.SkipFinalCompact {
		cfg.SkipFinalCompact = false
	}
	cfg.CompressFinal = true
}

func validateConfig(cfg Config) error {
	for _, p := range []string{cfg.WindowsISO, cfg.DriversDir, cfg.OVMFCode, cfg.OVMFVarsTemplate} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("required path not found: %s", p)
		}
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.Backend))
	if backend == "" {
		backend = "qemu"
	}
	if backend != "qemu" && backend != "libvirt" {
		return fmt.Errorf("unsupported backend %q, use qemu or libvirt", cfg.Backend)
	}
	profile := strings.ToLower(strings.TrimSpace(cfg.Profile))
	if profile != "" && profile != "default" && profile != "golden" {
		return fmt.Errorf("unsupported profile %q, use default or golden", cfg.Profile)
	}
	if backend == "libvirt" {
		if _, err := exec.LookPath("virt-install"); err != nil {
			return fmt.Errorf("backend=libvirt requires virt-install in PATH")
		}
		if _, err := exec.LookPath("virsh"); err != nil {
			return fmt.Errorf("backend=libvirt requires virsh in PATH")
		}
	}
	if cfg.CloudbaseInitMSI != "" {
		if _, err := os.Stat(cfg.CloudbaseInitMSI); err != nil {
			return fmt.Errorf("cloudbase-init msi path not found: %s", cfg.CloudbaseInitMSI)
		}
	}
	return nil
}

func getOSProfile(target string) (osProfile, error) {
	switch strings.ToLower(target) {
	case "win11", "windows11", "windows-11":
		return osProfile{
			Name:          "Windows 11",
			TemplatePath:  filepath.Join("templates", "autounattend.win11.xml.tmpl"),
			VirtioFlavor:  "w11",
			EnableTPM:     true,
			DriverFolders: []string{"viostor", "vioscsi", "NetKVM", "Balloon", "vioserial", "qemupciserial", "fwcfg", "viorng"},
		}, nil
	case "ws2025", "windows2025", "server2025", "windows-server-2025":
		return osProfile{
			Name:          "Windows Server 2025",
			TemplatePath:  filepath.Join("templates", "autounattend.ws2025.xml.tmpl"),
			VirtioFlavor:  "2k25",
			EnableTPM:     false,
			DriverFolders: []string{"viostor", "vioscsi", "NetKVM", "Balloon", "vioserial", "qemupciserial", "fwcfg", "viorng"},
		}, nil
	default:
		return osProfile{}, fmt.Errorf("unsupported os %q, use win11 or ws2025", target)
	}
}

func prepareDriverBundle(bc buildContext) error {
	if err := os.MkdirAll(bc.driverISORoot, 0o755); err != nil {
		return fmt.Errorf("create driver bundle root: %w", err)
	}

	for _, folder := range bc.osProfile.DriverFolders {
		src := filepath.Join(bc.cfg.DriversDir, folder, bc.osProfile.VirtioFlavor, "amd64")
		dst := filepath.Join(bc.driverISORoot, folder, bc.osProfile.VirtioFlavor, "amd64")
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("missing driver folder: %s", src)
		}
		if err := copyDir(src, dst); err != nil {
			return fmt.Errorf("copy driver folder %s: %w", folder, err)
		}
	}

	guestAgent := filepath.Join(bc.cfg.DriversDir, "guest-agent", "qemu-ga-x86_64.msi")
	if _, err := os.Stat(guestAgent); err == nil {
		if err := copyFile(guestAgent, filepath.Join(bc.driverISORoot, "qemu-ga-x86_64.msi")); err != nil {
			return fmt.Errorf("copy qemu guest agent MSI: %w", err)
		}
	}

	if bc.cfg.CloudbaseInitMSI != "" {
		if err := copyFile(bc.cfg.CloudbaseInitMSI, filepath.Join(bc.driverISORoot, "CloudbaseInitSetup.msi")); err != nil {
			return fmt.Errorf("copy cloudbase-init MSI: %w", err)
		}
	}

	marker := filepath.Join(bc.driverISORoot, "PACKAGER.TAG")
	if err := os.WriteFile(marker, []byte("windows-packager\n"), 0o644); err != nil {
		return fmt.Errorf("write bundle marker: %w", err)
	}
	return nil
}

func prepareDriverMedia(ctx context.Context, bc buildContext) error {
	cachePath := ""
	if bc.cfg.FastMode {
		key, err := computeDriverCacheKey(bc)
		if err != nil {
			return err
		}
		cachePath = filepath.Join(bc.cfg.WorkDir, ".cache", "driver-iso-"+key+".iso")
		if _, err := os.Stat(cachePath); err == nil {
			if err := copyFile(cachePath, bc.driverISOPath); err != nil {
				return fmt.Errorf("copy cached driver ISO: %w", err)
			}
			return nil
		}
	}

	if err := prepareDriverBundle(bc); err != nil {
		return err
	}
	if err := buildISO(ctx, bc.logWriter, bc.driverISORoot, bc.driverISOPath); err != nil {
		return fmt.Errorf("build driver ISO: %w", err)
	}

	if cachePath != "" {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err == nil {
			_ = copyFile(bc.driverISOPath, cachePath)
		}
	}
	return nil
}

func prepareAnswerMedia(bc buildContext) error {
	if err := os.MkdirAll(filepath.Join(bc.answerRoot, "scripts"), 0o755); err != nil {
		return fmt.Errorf("create answer media scripts dir: %w", err)
	}

	startupNSH := strings.Join([]string{
		"@echo -off",
		"map -r",
		"for %i run (0 1 2 3 4 5 6 7 8 9)",
		"  if exist fs%i:\\EFI\\BOOT\\BOOTX64.EFI then",
		"    fs%i:",
		"    \\EFI\\BOOT\\BOOTX64.EFI",
		"  endif",
		"  if exist fs%i:\\EFI\\Microsoft\\Boot\\bootmgfw.efi then",
		"    fs%i:",
		"    \\EFI\\Microsoft\\Boot\\bootmgfw.efi",
		"  endif",
		"endfor",
		"echo Failed to find Windows EFI bootloader",
		"reset -s",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(bc.answerRoot, "startup.nsh"), []byte(startupNSH), 0o644); err != nil {
		return fmt.Errorf("write startup.nsh: %w", err)
	}

	tplBytes, err := os.ReadFile(bc.osProfile.TemplatePath)
	if err != nil {
		return fmt.Errorf("read template %s: %w", bc.osProfile.TemplatePath, err)
	}
	tpl, err := template.New("autounattend").Parse(string(tplBytes))
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	data := templateData{
		AdminPassword:       bc.cfg.AdminPassword,
		WindowsEditionIndex: bc.cfg.WindowsEditionIndex,
		VirtioFlavor:        bc.osProfile.VirtioFlavor,
		AdminUsername:       bc.cfg.AdminUsername,
		EnableRDP:           bc.cfg.EnableRDP,
		OptimizeForSize:     bc.cfg.OptimizeForSize,
		GoldenProfile:       strings.EqualFold(strings.TrimSpace(bc.cfg.Profile), "golden"),
	}

	var rendered bytes.Buffer
	if err := tpl.Execute(&rendered, data); err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	if err := os.WriteFile(filepath.Join(bc.answerRoot, "Autounattend.xml"), rendered.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write autounattend: %w", err)
	}

	if err := copyFile(filepath.Join("scripts", "PostInstall.ps1"), filepath.Join(bc.answerRoot, "scripts", "PostInstall.ps1")); err != nil {
		return fmt.Errorf("copy PostInstall.ps1: %w", err)
	}
	if err := copyFile(filepath.Join("scripts", "Sysprep-Unattend.xml"), filepath.Join(bc.answerRoot, "scripts", "Sysprep-Unattend.xml")); err != nil {
		return fmt.Errorf("copy sysprep unattend: %w", err)
	}
	runPostInstallTmpl, err := os.ReadFile(filepath.Join("scripts", "RunPostInstall.cmd.tmpl"))
	if err != nil {
		return fmt.Errorf("read RunPostInstall template: %w", err)
	}
	runPostInstall, err := template.New("run-post-install").Parse(string(runPostInstallTmpl))
	if err != nil {
		return fmt.Errorf("parse RunPostInstall template: %w", err)
	}
	var runPostInstallRendered bytes.Buffer
	if err := runPostInstall.Execute(&runPostInstallRendered, data); err != nil {
		return fmt.Errorf("render RunPostInstall template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bc.answerRoot, "scripts", "RunPostInstall.cmd"), runPostInstallRendered.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write RunPostInstall.cmd: %w", err)
	}
	setupCompleteTmpl, err := os.ReadFile(filepath.Join("scripts", "SetupComplete.cmd.tmpl"))
	if err != nil {
		return fmt.Errorf("read SetupComplete template: %w", err)
	}
	setupComplete, err := template.New("setup-complete").Parse(string(setupCompleteTmpl))
	if err != nil {
		return fmt.Errorf("parse SetupComplete template: %w", err)
	}
	var setupCompleteRendered bytes.Buffer
	if err := setupComplete.Execute(&setupCompleteRendered, data); err != nil {
		return fmt.Errorf("render SetupComplete template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bc.answerRoot, "scripts", "SetupComplete.cmd"), setupCompleteRendered.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write SetupComplete.cmd: %w", err)
	}

	return nil
}

func buildISO(ctx context.Context, out io.Writer, srcDir, outISO string) error {
	if err := os.RemoveAll(outISO); err != nil {
		return fmt.Errorf("remove existing iso %s: %w", outISO, err)
	}
	if _, err := exec.LookPath("xorriso"); err == nil {
		return runCmd(ctx, out, "xorriso",
			"-as", "mkisofs",
			"-iso-level", "3",
			"-J", "-joliet-long",
			"-R",
			"-V", "CIDATA",
			"-o", outISO,
			srcDir,
		)
	}
	if _, err := exec.LookPath("genisoimage"); err == nil {
		return runCmd(ctx, out, "genisoimage",
			"-iso-level", "3",
			"-J", "-joliet-long",
			"-R",
			"-V", "CIDATA",
			"-o", outISO,
			srcDir,
		)
	}
	if _, err := exec.LookPath("mkisofs"); err == nil {
		return runCmd(ctx, out, "mkisofs",
			"-iso-level", "3",
			"-J", "-joliet-long",
			"-R",
			"-V", "CIDATA",
			"-o", outISO,
			srcDir,
		)
	}
	return fmt.Errorf("no ISO builder found: install one of xorriso, genisoimage, or mkisofs")
}

func runInstallVM(ctx context.Context, bc buildContext) error {
	backend := strings.ToLower(strings.TrimSpace(bc.cfg.Backend))
	if backend == "" {
		backend = "qemu"
	}
	switch backend {
	case "qemu":
		return runInstallVMQEMU(ctx, bc)
	case "libvirt":
		return runInstallVMLibvirt(ctx, bc)
	default:
		return fmt.Errorf("unsupported backend %q", bc.cfg.Backend)
	}
}

func runInstallVMQEMU(ctx context.Context, bc buildContext) error {
	accel, cpu := defaultQEMUCPUAndAccel()
	if bc.cfg.QemuAccel != "" {
		accel = bc.cfg.QemuAccel
	}
	if bc.cfg.QemuCPU != "" {
		cpu = bc.cfg.QemuCPU
	}

	if bc.osProfile.EnableTPM {
		if err := os.MkdirAll(bc.tpmDir, 0o755); err != nil {
			return fmt.Errorf("create swtpm dir: %w", err)
		}
	}

	var swtpmCmd *exec.Cmd
	if bc.osProfile.EnableTPM {
		swtpmArgs := []string{
			"socket",
			"--tpmstate", "dir=" + bc.tpmDir,
			"--ctrl", "type=unixio,path=" + bc.tpmSock,
			"--tpm2",
			"--log", "level=20",
		}
		swtpmCmd = exec.CommandContext(ctx, bc.cfg.SWTPMBin, swtpmArgs...)
		swtpmCmd.Stdout = bc.logWriter
		swtpmCmd.Stderr = bc.logWriter
		if err := swtpmCmd.Start(); err != nil {
			return fmt.Errorf("start swtpm: %w", err)
		}
		defer func() {
			if swtpmCmd.Process != nil {
				_ = swtpmCmd.Process.Kill()
				_, _ = swtpmCmd.Process.Wait()
			}
		}()

		for i := 0; i < 50; i++ {
			if _, err := os.Stat(bc.tpmSock); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if _, err := os.Stat(bc.tpmSock); err != nil {
			return fmt.Errorf("swtpm socket not ready: %w", err)
		}
	}

	diskDrive := fmt.Sprintf("file=%s,if=none,id=osdisk,format=qcow2,discard=unmap", bc.diskPath)
	if bc.cfg.FastMode {
		diskDrive = fmt.Sprintf("file=%s,if=none,id=osdisk,format=qcow2,discard=unmap,cache=writeback,aio=threads", bc.diskPath)
	}
	winISO := fmt.Sprintf("file=%s,if=none,id=winiso,media=cdrom,readonly=on", bc.cfg.WindowsISO)
	answerISO := fmt.Sprintf("file=%s,if=none,id=answeriso,media=cdrom,readonly=on", bc.answerISOPath)
	driverISO := fmt.Sprintf("file=%s,if=none,id=driveriso,media=cdrom,readonly=on", bc.driverISOPath)

	args := []string{
		"-machine", "q35,accel=" + accel,
		"-cpu", cpu,
		"-smp", fmt.Sprintf("%d", bc.cfg.CPUs),
		"-m", fmt.Sprintf("%d", bc.cfg.MemoryMB),
		"-boot", "order=dc,menu=on",
		"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", bc.cfg.OVMFCode),
		"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", bc.ovmfVarsPath),
		"-device", "ich9-ahci,id=ahci0",
		"-drive", diskDrive,
		"-drive", winISO,
		"-drive", answerISO,
		"-drive", driverISO,
		"-device", "ide-hd,drive=osdisk,bus=ahci0.0,bootindex=3",
		"-device", "ide-cd,drive=answeriso,bus=ahci0.2,bootindex=1",
		"-device", "ide-cd,drive=winiso,bus=ahci0.1,bootindex=2",
		"-device", "ide-cd,drive=driveriso,bus=ahci0.3",
		"-device", "virtio-net-pci",
	}

	if bc.osProfile.EnableTPM {
		args = append(args,
			"-chardev", "socket,id=chrtpm,path="+bc.tpmSock,
			"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
			"-device", "tpm-tis,tpmdev=tpm0",
		)
	}

	if strings.TrimSpace(bc.cfg.VNCListen) != "" {
		args = append(args,
			"-display", "none",
			"-vnc", bc.cfg.VNCListen,
		)
	} else if bc.cfg.Headless {
		args = append(args, "-nographic")
	}

	vmCmd := exec.CommandContext(ctx, bc.cfg.QemuSystemBin, args...)
	vmCmd.Stdout = bc.logWriter
	vmCmd.Stderr = bc.logWriter
	if bc.cfg.Interactive {
		vmCmd.Stdin = os.Stdin
	}

	if err := vmCmd.Run(); err != nil {
		if actualSize, infoErr := qcow2ActualSize(ctx, bc.cfg.QemuImgBin, bc.diskPath); infoErr == nil && actualSize > (2*1024*1024*1024) {
			fmt.Fprintf(bc.logWriter, "warning: qemu exited non-zero (%v), but qcow2 actual-size is %d bytes; continuing\n", err, actualSize)
			return nil
		}
		return fmt.Errorf("qemu run failed: %w", err)
	}

	actualSize, err := qcow2ActualSize(ctx, bc.cfg.QemuImgBin, bc.diskPath)
	if err != nil {
		return fmt.Errorf("inspect created qcow2: %w", err)
	}
	if actualSize < (512 * 1024 * 1024) {
		return fmt.Errorf("created qcow2 appears empty (actual-size=%d bytes); refusing output", actualSize)
	}

	return nil
}

func runInstallVMLibvirt(ctx context.Context, bc buildContext) error {
	_ = runCmd(ctx, bc.logWriter, "virsh", "destroy", bc.vmName)
	_ = runCmd(ctx, bc.logWriter, "virsh", "undefine", bc.vmName, "--nvram")

	vncListen := "127.0.0.1"
	if strings.TrimSpace(bc.cfg.VNCListen) != "" {
		if host := strings.TrimSpace(strings.SplitN(bc.cfg.VNCListen, ":", 2)[0]); host != "" {
			vncListen = host
		}
	}
	osVariant := "win11"
	if strings.Contains(strings.ToLower(bc.cfg.OS), "2025") || strings.Contains(strings.ToLower(bc.cfg.OS), "ws") {
		osVariant = "win2k22"
	}

	args := []string{
		"--name", bc.vmName,
		"--memory", strconv.Itoa(bc.cfg.MemoryMB),
		"--vcpus", strconv.Itoa(bc.cfg.CPUs),
		"--cpu", "host",
		"--machine", "q35",
		"--boot", "uefi",
		"--disk", fmt.Sprintf("path=%s,format=qcow2,bus=virtio,discard=unmap", bc.diskPath),
		"--disk", fmt.Sprintf("path=%s,device=cdrom,bus=sata", bc.cfg.WindowsISO),
		"--disk", fmt.Sprintf("path=%s,device=cdrom,bus=sata", bc.answerISOPath),
		"--disk", fmt.Sprintf("path=%s,device=cdrom,bus=sata", bc.driverISOPath),
		"--network", "network=default,model=virtio",
		"--graphics", "vnc,listen=" + vncListen,
		"--video", "virtio",
		"--os-variant", osVariant,
		"--noautoconsole",
		"--wait", "-1",
	}
	if err := runCmd(ctx, bc.logWriter, "virt-install", args...); err != nil {
		if actualSize, infoErr := qcow2ActualSize(ctx, bc.cfg.QemuImgBin, bc.diskPath); infoErr == nil && actualSize > (2*1024*1024*1024) {
			fmt.Fprintf(bc.logWriter, "warning: virt-install exited non-zero (%v), but qcow2 actual-size is %d bytes; continuing\n", err, actualSize)
			return nil
		}
		return fmt.Errorf("virt-install run failed: %w", err)
	}

	actualSize, err := qcow2ActualSize(ctx, bc.cfg.QemuImgBin, bc.diskPath)
	if err != nil {
		return fmt.Errorf("inspect created qcow2: %w", err)
	}
	if actualSize < (512 * 1024 * 1024) {
		return fmt.Errorf("created qcow2 appears empty (actual-size=%d bytes); refusing output", actualSize)
	}
	return nil
}

func qcow2ActualSize(ctx context.Context, qemuImgBin, imagePath string) (int64, error) {
	cmd := exec.CommandContext(ctx, qemuImgBin, "info", "--output=json", imagePath)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var info struct {
		ActualSize int64 `json:"actual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, err
	}
	if info.ActualSize <= 0 {
		return 0, fmt.Errorf("actual-size missing in qemu-img info output")
	}
	return info.ActualSize, nil
}

func defaultQEMUCPUAndAccel() (accel string, cpu string) {
	if runtime.GOOS == "linux" {
		if runtime.GOARCH == "amd64" {
			return "kvm:tcg", "host"
		}
		return "tcg", "max"
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "tcg", "max"
	}
	return "hvf:tcg", "host"
}

func compactImage(ctx context.Context, bc buildContext) error {
	if err := os.MkdirAll(filepath.Dir(bc.finalOutputPath), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	if bc.cfg.SkipFinalCompact {
		if bc.diskPath == bc.finalOutputPath {
			return nil
		}
		if err := moveFile(bc.diskPath, bc.finalOutputPath); err != nil {
			return fmt.Errorf("skip compact move output: %w", err)
		}
		return nil
	}

	args := []string{
		"convert",
		"-O", "qcow2",
	}
	if bc.cfg.QemuImgConvertThreads > 0 {
		args = append(args, "-m", strconv.Itoa(bc.cfg.QemuImgConvertThreads))
	}
	if bc.cfg.CompressFinal {
		args = append(args, "-c")
	}
	args = append(args,
		"-o", "cluster_size=65536,lazy_refcounts=on",
		bc.diskPath,
		bc.finalOutputPath,
	)
	if err := runCmd(ctx, bc.logWriter, bc.cfg.QemuImgBin, args...); err != nil {
		return fmt.Errorf("compact image: %w", err)
	}
	return nil
}

func runCmd(ctx context.Context, out io.Writer, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func resolveExecutable(bin string, fallbackPaths []string) string {
	if strings.TrimSpace(bin) == "" {
		return bin
	}
	if filepath.IsAbs(bin) {
		return bin
	}
	if _, err := exec.LookPath(bin); err == nil {
		return bin
	}
	for _, p := range fallbackPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return bin
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func computeDriverCacheKey(bc buildContext) (string, error) {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write("os", bc.cfg.OS, "virtio", bc.osProfile.VirtioFlavor)

	for _, folder := range bc.osProfile.DriverFolders {
		src := filepath.Join(bc.cfg.DriversDir, folder, bc.osProfile.VirtioFlavor, "amd64")
		if err := hashTreeMeta(h, src); err != nil {
			return "", fmt.Errorf("hash driver folder %s: %w", src, err)
		}
	}

	guestAgent := filepath.Join(bc.cfg.DriversDir, "guest-agent", "qemu-ga-x86_64.msi")
	if err := hashPathMeta(h, guestAgent); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if bc.cfg.CloudbaseInitMSI != "" {
		if err := hashPathMeta(h, bc.cfg.CloudbaseInitMSI); err != nil {
			return "", err
		}
	}

	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16]), nil
}

func hashTreeMeta(h io.Writer, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(h, rel)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, fi.Mode().String())
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, strconv.FormatInt(fi.Size(), 10))
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, strconv.FormatInt(fi.ModTime().UnixNano(), 10))
		_, _ = io.WriteString(h, "\x00")
		return nil
	})
}

func hashPathMeta(h io.Writer, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(h, path)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, strconv.FormatInt(fi.Size(), 10))
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, strconv.FormatInt(fi.ModTime().UnixNano(), 10))
	_, _ = io.WriteString(h, "\x00")
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
