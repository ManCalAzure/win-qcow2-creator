# Windows Packager

Builds unattended Windows qcow2 images (Windows 11 / Server 2025) from a Linux host using QEMU/KVM. Includes a built-in web UI and runs headlessly in Docker.

```
┌─────────────────────────────────────────────────────────────────────┐
│                        HOW IT WORKS                                 │
│                                                                     │
│   Your Host Machine                                                 │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │  ./data/  (or /opt/windows-packager/data/)                   │   │
│  │    ├── Win11.iso              ← you provide                  │   │
│  │    ├── CloudbaseInit.msi      ← you provide (optional)       │   │
│  │    └── build/                                                │   │
│  │          └── win11.qcow2     ← packager writes here          │   │
│  └──────────────────────────────────────────────────────────────┘   │
│           │  /data volume mount                 ▲                   │
│           ▼                                     │                   │
│  ┌─────────────────────────────────────────┐    │                   │
│  │        Docker Container                 │    │                   │
│  │  ┌──────────────┐  ┌────────────────┐   │    │                   │
│  │  │  Web UI      │  │  QEMU/KVM VM   │───┼────┘                   │
│  │  │  :8080  ─────┼──▶  Windows ISO   │   │                        │
│  │  │              │  │  VirtIO drivers│   │                        │
│  │  │  VNC proxy   │  │  Autounattend  │   │                        │
│  │  │  :5900  ─────┼──▶  (headless)   │   │                        │
│  │  └──────────────┘  └────────────────┘   │                        │
│  └─────────────────────────────────────────┘                        │
│           │                      │                                  │
│      Browser                 VNC Viewer                             │
│   http://localhost:8080    localhost:5900                           │
└─────────────────────────────────────────────────────────────────────┘
```

## Requirements

| | |
|---|---|
| **OS** | Linux x86_64 |
| **Docker** | Any recent version |
| **KVM** | `/dev/kvm` must exist — enable Intel VT-x or AMD-V in BIOS |
| **Port 8080** | Web UI |
| **Port 5900** | VNC console (watch the install live) |
| **`/data` mount** | Required — ISOs go in, qcow2 images come out |

---

## Quick Start — `docker run` (laptop / single machine)

**Step 1 — Create your data directory and drop in your ISO:**

```bash
mkdir -p ~/windows-packager/data
cp /path/to/Win11.iso ~/windows-packager/data/
# optional:
cp /path/to/CloudbaseInitSetup_x64.msi ~/windows-packager/data/
```

**Step 2 — Run the container:**

```bash
docker run -d \
  --name windows-packager \
  --restart unless-stopped \
  --device /dev/kvm:/dev/kvm \
  -p 8080:8080 \
  -p 5900:5900 \
  -v ~/windows-packager/data:/data \
  zedmanny/windows-packager:latest
```

**Step 3 — Open the UI:**

```
http://localhost:8080
```

Point the ISO field to `/data/Win11.iso`, click **Build**, and watch progress stream in the browser.
Connect a VNC viewer to `localhost:5900` to see the Windows installer in real time.

**Step 4 — Get your image:**

```bash
ls ~/windows-packager/data/build/
# win11.qcow2   ← your finished image
```

---

## Quick Start — `docker compose` (clone + run)

```bash
git clone https://github.com/ManCalAzure/win-qcow2-creator-for-ubuntu.git
cd win-qcow2-creator-for-ubuntu

mkdir -p data
cp /path/to/Win11.iso data/

docker compose up -d
```

Open `http://localhost:8080`.

---

## Data Volume Layout

```
/data/                              (mounted from host)
  ├── Win11.iso                     ← Windows 11 ISO  (you provide)
  ├── WinServer2025.iso             ← WS2025 ISO      (you provide, optional)
  ├── CloudbaseInitSetup_x64.msi   ← Cloudbase-Init   (optional)
  └── build/
        ├── win11.qcow2            ← output image
        └── ws2025.qcow2           ← output image
```

---

## Web UI Defaults

| Setting | Value |
|---|---|
| OVMF Code | `/usr/share/OVMF/OVMF_CODE_4M.fd` |
| OVMF Vars | `/usr/share/OVMF/OVMF_VARS_4M.fd` |
| swtpm | `/usr/bin/swtpm` |
| VirtIO drivers | `/app/drivers` |
| Work dir | `/data/build` |
| VNC | `0.0.0.0:0` → port 5900 |

---

## Headless CLI Builds (inside running container)

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

---

## Golden Profile (cloud/production images)

Use `-profile golden` to produce a production-ready cloud image:

- Enables RDP
- Optimize-for-size pass inside guest
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

---

## Compressing the Output Image

Compression is skipped by default for speed. To shrink the final image manually:

```bash
qemu-img convert -p -O qcow2 -c \
  ~/windows-packager/data/build/win11.qcow2 \
  ~/windows-packager/data/build/win11.min.qcow2
```

---

## Building from Source

```bash
git clone https://github.com/ManCalAzure/win-qcow2-creator-for-ubuntu.git
cd win-qcow2-creator-for-ubuntu

# Build and push Docker image
docker build -t zedmanny/windows-packager:latest .
docker push zedmanny/windows-packager:latest

# Native build (Ubuntu 24.04)
sudo apt install -y qemu-system-x86 qemu-utils ovmf swtpm swtpm-tools xorriso mtools
go build -o windows-packager ./cmd/windows-packager
./windows-packager -ui -ui-listen 0.0.0.0:8080
```

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| Very slow build | Ensure `/dev/kvm` is passed to the container (`--device /dev/kvm`) |
| KVM unavailable | Drop `--device /dev/kvm` — QEMU falls back to TCG (much slower) |
| `xorriso` not found | Already bundled in the Docker image; native install: `apt install xorriso` |
| swtpm error | Bundled in Docker image; native: verify with `which swtpm` |
| OVMF not found | Bundled in Docker image; native: check `/usr/share/OVMF/` |

---

## Notes

- Windows 11 unattended install includes LabConfig bypasses for TPM, SecureBoot, CPU, and RAM checks — no modifications needed.
- The packager automatically dismisses the UEFI "Press any key to boot from CD or DVD" prompt via QMP so headless builds proceed without human interaction.
- Automation uses `SetupComplete.cmd` with scripts injected into `C:\Windows\Temp\Packager`.
- `-backend libvirt` uses `virt-install`/`virsh` instead of direct QEMU — requires libvirt on the host.
