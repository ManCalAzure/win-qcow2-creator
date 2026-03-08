# Windows Packager

Builds unattended Windows qcow2 images (Windows 11 / Server 2025) from a Linux host using QEMU/KVM. Includes a built-in web UI and runs headlessly in Docker.

## Container Requirements

| Requirement | Details |
|-------------|---------|
| **Port 8080** | Web UI — must be exposed/mapped to access the browser interface |
| **Port 5900** | VNC console — expose to watch the Windows install live |
| **`/data` volume** | **Required.** Mount a host directory here. Windows ISOs and optional MSIs go in, finished qcow2 images come out. The container cannot build without this mount. |
| **`/dev/kvm`** | Strongly recommended. Pass through for hardware-accelerated builds. Without it, QEMU falls back to TCG (software emulation, 5–10× slower). |

## Quick Start (Docker — recommended)

### Portainer Stack

Create a new stack in Portainer and paste the contents of [`portainer-stack.yml`](portainer-stack.yml):

```yaml
services:
  windows-packager:
    image: zedmanny/windows-packager:latest
    container_name: windows-packager
    restart: unless-stopped
    ports:
      - "8080:8080"   # Web UI — open http://<host-ip>:8080
      - "5900:5900"   # VNC console — connect with any VNC viewer
    devices:
      - /dev/kvm:/dev/kvm   # Remove if KVM unavailable (slower TCG fallback)
    volumes:
      - /opt/windows-packager/data:/data   # Place ISOs here; images written here
```

Create the data directory on the host before deploying:

```bash
sudo mkdir -p /opt/windows-packager/data
# Then copy your ISOs:
sudo cp Win11.iso /opt/windows-packager/data/
sudo cp CloudbaseInitSetup_x64.msi /opt/windows-packager/data/   # optional
```

### docker-compose

```bash
git clone https://github.com/ManCalAzure/win-qcow2-creator-for-ubuntu.git
cd win-qcow2-creator-for-ubuntu
mkdir -p data
# Copy ISOs into ./data/ before starting
cp /path/to/Win11.iso data/
docker compose up -d
```

Open `http://<host-ip>:8080` — **port 8080 must be reachable from your browser**.
Connect a VNC viewer to `<host-ip>:5900` to watch the install.

## Portability

The Docker image (`zedmanny/windows-packager:latest`) runs on any Linux x86_64 host with:

- Docker (or compatible runtime)
- `/dev/kvm` available (Intel VT-x or AMD-V enabled in BIOS)

If `/dev/kvm` is unavailable, remove the `devices:` block — QEMU will fall back to TCG (software emulation, much slower).

## Data Volume Layout

Mount a host directory to `/data` inside the container. Place input files there and finished images are written there:

```
/data/
  Win11.iso                    # Windows 11 installation ISO
  WinServer2025.iso            # Windows Server 2025 ISO (optional)
  CloudbaseInitSetup_x64.msi   # Cloudbase-Init installer (optional)
  build/
    win11.qcow2                # Output image (written by packager)
    ws2025.qcow2
```

## Web UI

Open `http://<host-ip>:8080` after the container starts.

- Select OS, point to ISO and optional MSI paths under `/data/`
- Click **Build** — progress streams in the browser
- Connect to VNC at `<host-ip>:5900` to watch the install live

Container defaults:

| Setting      | Value                                  |
|--------------|----------------------------------------|
| OVMF Code    | `/usr/share/OVMF/OVMF_CODE_4M.fd`     |
| OVMF Vars    | `/usr/share/OVMF/OVMF_VARS_4M.fd`     |
| swtpm        | `/usr/bin/swtpm`                       |
| VirtIO drivers | `/app/drivers`                       |
| Work dir     | `/data/build`                          |
| VNC          | `0.0.0.0:0` (port 5900)               |

## CLI Examples (inside container)

Windows 11:

```bash
docker exec windows-packager /app/windows-packager \
  -os win11 \
  -win-iso /data/Win11.iso \
  -drivers-dir /app/drivers \
  -cloudbase-msi /data/CloudbaseInitSetup_x64.msi \
  -output /data/build/win11.qcow2 \
  -workdir /data/build \
  -vnc 0.0.0.0:0 \
  -headless true
```

Windows Server 2025:

```bash
docker exec windows-packager /app/windows-packager \
  -os ws2025 \
  -win-iso /data/WinServer2025.iso \
  -drivers-dir /app/drivers \
  -cloudbase-msi /data/CloudbaseInitSetup_x64.msi \
  -output /data/build/ws2025.qcow2 \
  -workdir /data/build \
  -vnc 0.0.0.0:0 \
  -headless true
```

## Golden Profile

Use `-profile golden` for production/cloud image builds:

- Enables RDP
- Enables optimize-for-size stage in guest
- Forces final compact + compression
- Applies Cloudbase-Init NoCloud plugin defaults
- Installs `virtio-win-gt-x64.msi` if present on driver media

```bash
docker exec windows-packager /app/windows-packager \
  -profile golden \
  -os ws2025 \
  -win-iso /data/WinServer2025.iso \
  -drivers-dir /app/drivers \
  -cloudbase-msi /data/CloudbaseInitSetup_x64.msi \
  -output /data/build/ws2025-golden.qcow2 \
  -workdir /data/build
```

## Compressing the Output Image

The packager skips final compression by default (`-skip-compact=true`). To compress manually:

```bash
qemu-img convert -p -O qcow2 -c /data/build/win11.qcow2 /data/build/win11.min.qcow2
```

## Building from Source

```bash
git clone https://github.com/ManCalAzure/win-qcow2-creator-for-ubuntu.git
cd win-qcow2-creator-for-ubuntu

# Build Docker image and push
docker build -t zedmanny/windows-packager:latest .
docker push zedmanny/windows-packager:latest

# Or build and run natively (Ubuntu 24.04)
sudo apt install -y qemu-system-x86 qemu-utils ovmf swtpm swtpm-tools xorriso mtools
go build -o windows-packager ./cmd/windows-packager
./windows-packager -ui -ui-listen 0.0.0.0:8080
```

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `xorriso` not found | Install `xorriso` (`apt install xorriso`) |
| swtpm error | Verify path with `which swtpm`; pass `-swtpm <path>` |
| OVMF not found | Check `/usr/share/OVMF/`; override with `-ovmf-code` / `-ovmf-vars` |
| Very slow build | Ensure `/dev/kvm` is passed through; check `kvm-ok` on host |
| KVM unavailable | Remove `devices:` block from compose/stack — falls back to TCG |

## Notes

- Windows 11 unattended install includes LabConfig registry bypasses for TPM, SecureBoot, CPU, and RAM checks.
- The packager automatically handles the UEFI "Press any key to boot from CD or DVD" prompt via QMP.
- Automation uses `SetupComplete.cmd` and scripts injected into `C:\Windows\Temp\Packager`.
- `-backend libvirt` uses `virt-install`/`virsh` instead of direct QEMU — requires libvirt on the host.
