# ubuntu-app (Windows Packager for Ubuntu 24)

This is a Linux-focused equivalent of the Windows packager app.
It builds unattended Windows qcow2 images (Win11 / Server 2025) from Ubuntu 24.

## What it does

- Unattended Windows install from ISO
- Adds VirtIO drivers from local `drivers/`
- Optional Cloudbase-Init install
- Optional QEMU guest agent install
- Optional RDP enablement
- Sysprep + shutdown
- Final compressed qcow2 output (compression may fail just run manually - ```qemu-img convert -p -O qcow2 -c win11.qcow2 win11.min.qcow2``` ) 
- Built-in web UI (`-ui`)

## Ubuntu 24 prerequisites

```bash
sudo apt update
sudo apt install -y qemu-system-x86 qemu-utils ovmf swtpm swtpm-tools xorriso
```

Optional ISO tools (fallbacks):
- `genisoimage` or `mkisofs`
- `libvirt-clients` + `virt-install` if using `-backend libvirt`

## Expected local inputs

- `Win11.iso` and/or `WinServer2025.iso` <---- in main dir
- `drivers/` (VirtIO extracted driver tree) <- in main dir
- Optional `CloudbaseInitSetup_x64.msi` <----- in main dir

## Build

```bash
git clone https://github.com/ManCalAzure/win-qcow2-creator-for-ubuntu.git
cd win-qcow2-creator-for-ubuntu
go build -o windows-packager ./cmd/windows-packager
```

## Run UI

```bash
./windows-packager -ui -ui-listen 0.0.0.0:8080
```

Open `http://<ip address of host>:8080`.

Ubuntu defaults in UI/CLI:
- `OVMF Code`: `/usr/share/OVMF/OVMF_CODE.fd`
- `OVMF Vars`: `/usr/share/OVMF/OVMF_VARS.fd`
- `SWTPM`: `/usr/bin/swtpm`

## CLI example (Windows 11)

```bash
./windows-packager \
  -backend qemu \
  -profile default \
  -os win11 \
  -win-iso ./Win11.iso \
  -drivers-dir ./drivers \
  -cloudbase-msi ./CloudbaseInitSetup_x64.msi \
  -output ./build/win11.qcow2 \
  -vnc 0.0.0.0:0 \
  -ovmf-code /usr/share/OVMF/OVMF_CODE.fd \
  -ovmf-vars /usr/share/OVMF/OVMF_VARS.fd \
  -swtpm /usr/bin/swtpm \
  -memory 6144
```

## CLI example (Windows Server 2025)

```bash
./windows-packager \
  -backend qemu \
  -profile default \
  -os ws2025 \
  -win-iso ./WinServer2025.iso \
  -drivers-dir ./drivers \
  -cloudbase-msi ./CloudbaseInitSetup_x64.msi \
  -output ./build/ws2025.qcow2 \
  -vnc :1 \
  -ovmf-code /usr/share/OVMF/OVMF_CODE.fd \
  -ovmf-vars /usr/share/OVMF/OVMF_VARS.fd
```

## Golden profile

Use `-profile golden` for a gold-image oriented build preset:
- Enables RDP
- Enables optimize-for-size stage in guest
- Forces final compact + compression
- Applies Cloudbase-Init NoCloud plugin defaults
- Installs `virtio-win-gt-x64.msi` if present on driver media

Example:

```bash
./windows-packager \
  -backend libvirt \
  -profile golden \
  -os ws2025 \
  -win-iso ./WinServer2025.iso \
  -drivers-dir ./drivers \
  -cloudbase-msi ./CloudbaseInitSetup_x64.msi \
  -output ./build/ws2025-golden.qcow2 \
  -vm-name ws2025-golden-build
```

## Notes

- On Linux x86_64, default accelerator is `kvm:tcg` and CPU model `host`.
- If `/dev/kvm` is unavailable, QEMU falls back to TCG (slower).
- Speed defaults are enabled:
  - `-fast=true` (driver ISO cache + faster disk I/O profile)
  - `-skip-compact=true` (final `qemu-img convert` is skipped)
- To get a smaller final image (slower), run with:
  - `-skip-compact=false -compress=true`
- Win11 unattended includes LabConfig bypass keys for TPM/SecureBoot/CPU/RAM checks.
- Automation trigger uses `SetupComplete.cmd` and local `C:\Windows\Temp\Packager` scripts.
- Use `-vnc <addr:display>` for remote console during build. Example: `-vnc 127.0.0.1:1`.
- If `-vnc` is set, QEMU uses `-display none -vnc ...` (takes precedence over `-headless`).
- `-backend libvirt` uses `virt-install`/`virsh` instead of direct QEMU args.

## Troubleshooting

- If build fails with ISO tool error, install `xorriso`.
- If build fails with swtpm path, verify:
  - `which swtpm`
  - set `-swtpm` to the correct path.
- If OVMF files differ on your distro image, override `-ovmf-code` / `-ovmf-vars`.
