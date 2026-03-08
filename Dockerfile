# ============================================================
# Stage 1: Build the Go binary
# ============================================================
FROM golang:1.23-bookworm AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /windows-packager ./cmd/windows-packager

# ============================================================
# Stage 2: Runtime image
# ============================================================
FROM ubuntu:24.04

# Install QEMU + supporting tools (no large GUI packages)
RUN apt-get update && apt-get install -y --no-install-recommends \
        qemu-system-x86 \
        qemu-utils \
        ovmf \
        swtpm \
        swtpm-tools \
        xorriso \
        mtools \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the compiled binary
COPY --from=builder /windows-packager /app/windows-packager

# Copy templates and scripts (read at runtime via relative paths)
COPY templates/ /app/templates/
COPY scripts/   /app/scripts/

# Copy the bundled virtio driver tree (small .inf/.cat/.sys files, no ISOs)
COPY drivers/ /app/drivers/

# /data is where the user mounts Windows ISOs, Cloudbase-Init MSI,
# and receives the finished qcow2 output.
VOLUME ["/data"]

# Web UI port
EXPOSE 8080

# Default: start the web UI.
# ISOs are expected at /data (mount the host directory that holds them).
# Override with CLI flags or pass a different CMD for headless builds.
ENTRYPOINT ["/app/windows-packager"]
CMD ["-ui", "-ui-listen", "0.0.0.0:8080", \
     "-drivers-dir", "/app/drivers", \
     "-workdir",     "/data/build"]
