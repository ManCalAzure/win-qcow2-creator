package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"ubuntu-app/internal/packager"
	"ubuntu-app/internal/ui"
)

func main() {
	var cfg packager.Config
	var uiMode bool
	var uiListen string

	flag.StringVar(&cfg.Backend, "backend", "qemu", "Install backend: qemu or libvirt")
	flag.StringVar(&cfg.Profile, "profile", "default", "Build profile: default or golden")
	flag.StringVar(&cfg.VMName, "vm-name", "", "VM/domain name override (auto if empty)")
	flag.StringVar(&cfg.OS, "os", "win11", "Target OS: win11 or ws2025")
	flag.StringVar(&cfg.WindowsISO, "win-iso", "", "Path to Windows ISO")
	flag.StringVar(&cfg.DriversDir, "drivers-dir", "./drivers", "Path to extracted virtio drivers directory")
	flag.StringVar(&cfg.CloudbaseInitMSI, "cloudbase-msi", "", "Path to Cloudbase-Init MSI (optional)")
	flag.StringVar(&cfg.OutputImage, "output", "", "Output qcow2 path (default: ./build/<os>.qcow2)")
	flag.StringVar(&cfg.WorkDir, "workdir", "./build", "Working directory")

	flag.StringVar(&cfg.DiskSize, "disk-size", "40G", "Initial qcow2 size")
	flag.IntVar(&cfg.CPUs, "cpus", 4, "vCPU count")
	flag.IntVar(&cfg.MemoryMB, "memory", 8192, "Memory in MB")
	flag.BoolVar(&cfg.Headless, "headless", false, "Run QEMU without graphical display")
	flag.StringVar(&cfg.VNCListen, "vnc", "", "Enable QEMU VNC console (example: 127.0.0.1:1 or :1)")
	flag.BoolVar(&cfg.FastMode, "fast", true, "Enable faster build defaults (driver ISO cache + faster disk I/O)")

	flag.StringVar(&cfg.QemuSystemBin, "qemu-system", "qemu-system-x86_64", "Path to qemu-system-x86_64 binary")
	flag.StringVar(&cfg.QemuImgBin, "qemu-img", "qemu-img", "Path to qemu-img binary")

	defaultSwtpm := "/usr/bin/swtpm"
	if path, err := exec.LookPath("swtpm"); err == nil {
		defaultSwtpm = path
	}
	flag.StringVar(&cfg.SWTPMBin, "swtpm", defaultSwtpm, "Path to swtpm binary")
	flag.StringVar(&cfg.QemuAccel, "qemu-accel", "", "QEMU accelerator override (auto if empty)")
	flag.StringVar(&cfg.QemuCPU, "qemu-cpu", "", "QEMU CPU model override (auto if empty)")
	flag.StringVar(&cfg.OVMFCode, "ovmf-code", "/usr/share/OVMF/OVMF_CODE_4M.fd", "Path to OVMF code fd")
	flag.StringVar(&cfg.OVMFVarsTemplate, "ovmf-vars", "/usr/share/OVMF/OVMF_VARS_4M.fd", "Path to OVMF vars template fd")
	flag.BoolVar(&cfg.SkipFinalCompact, "skip-compact", true, "Skip qemu-img convert and keep the install qcow2 as final output")
	flag.BoolVar(&cfg.CompressFinal, "compress", false, "Compress final qcow2 during convert (slower, smaller)")
	flag.IntVar(&cfg.QemuImgConvertThreads, "img-convert-threads", 0, "Threads for qemu-img convert (0 = auto from vCPU count)")

	flag.IntVar(&cfg.WindowsEditionIndex, "edition-index", 1, "Windows image index inside install.wim/esd")
	flag.StringVar(&cfg.AdminUsername, "admin-username", "Administrator", "Local admin username")
	flag.StringVar(&cfg.AdminPassword, "admin-password", "P@ssw0rd!", "Administrator password for unattended install")
	flag.BoolVar(&cfg.EnableRDP, "enable-rdp", false, "Enable Remote Desktop in post-install")
	flag.BoolVar(&cfg.OptimizeForSize, "optimize-size", false, "Zero free space before sysprep for smaller final qcow2 (slower)")
	flag.BoolVar(&uiMode, "ui", false, "Run built-in web UI")
	flag.StringVar(&uiListen, "ui-listen", "127.0.0.1:8080", "Web UI listen address")
	flag.Parse()

	if uiMode {
		if err := ui.Run(uiListen); err != nil {
			log.Fatal(err)
		}
		return
	}

	if cfg.WindowsISO == "" {
		log.Fatal("-win-iso is required")
	}

	if _, err := os.Stat(cfg.DriversDir); os.IsNotExist(err) {
		log.Fatalf("Drivers directory not found: %s", cfg.DriversDir)
	}
	// Validate that the drivers directory actually contains driver subfolders
	if _, err := os.Stat(filepath.Join(cfg.DriversDir, "NetKVM")); os.IsNotExist(err) {
		log.Printf("WARNING: 'NetKVM' folder not found in %s.", cfg.DriversDir)
		log.Printf("         Ensure you are pointing to the extracted virtio-win ISO root.")
	}

	log.Printf("Configuration verified:")
	log.Printf("  OS: %s", cfg.OS)
	log.Printf("  ISO: %s", cfg.WindowsISO)
	log.Printf("  Drivers: %s", cfg.DriversDir)
	log.Printf("  Output: %s", cfg.OutputImage)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if cfg.OutputImage == "" {
		cfg.OutputImage = filepath.Join(cfg.WorkDir, fmt.Sprintf("%s.qcow2", cfg.OS))
	}
	cfg.Interactive = true

	if err := packager.Run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
