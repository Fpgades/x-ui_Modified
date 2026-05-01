# Dev VPS Bootstrap

One-shot script for a fresh Ubuntu 22.04/24.04 VPS to be used as the
xui-mesh development & build machine. Targets:

- Go 1.26 toolchain
- protoc + Go gRPC plugins
- build tools, git, tmux
- 2GB swap if RAM < 2GB
- repo clone with PAT-based credential helper

Recommended hardware: ≥ 2 GB RAM, ≥ 10 GB free disk. Smaller works but
sluggish.

---

## Step 1 — preflight

Connect as root (or sudo) and check:

```bash
free -h
df -h /
cat /etc/os-release | head -3
```

You want at least ~5 GB free on `/` and Ubuntu 22.04 or 24.04.

## Step 2 — packages, swap, Go, protoc (one block)

Copy-paste this whole block. Safe to run on a clean machine. Idempotent
(re-running won't break anything).

```bash
set -e

# --- packages ---
apt-get update -qq
apt-get install -y -qq build-essential git protobuf-compiler tmux curl ca-certificates

# --- swap (2GB) if no swap configured ---
if [ -z "$(swapon --show)" ]; then
  fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048
  chmod 600 /swapfile
  mkswap /swapfile
  swapon /swapfile
  grep -q '/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

# --- Go 1.26 ---
GO_VER=1.26.2
cd /tmp
curl -fsSL https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz -o go.tgz
rm -rf /usr/local/go
tar -C /usr/local -xzf go.tgz
rm go.tgz
ln -sf /usr/local/go/bin/go     /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt  /usr/local/bin/gofmt

# --- protoc Go plugins ---
GOBIN=/usr/local/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
GOBIN=/usr/local/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# --- verify ---
echo "--- versions ---"
go version
protoc --version
which protoc-gen-go protoc-gen-go-grpc
free -h | head -2
df -h / | tail -1
```

If `Go 1.26.2` 404s (newer release shipped), bump `GO_VER` to whatever's
on https://go.dev/dl. Anything ≥ 1.24 works (toolchain auto-pull will
pick the rest).

## Step 3 — clone the private repo

Create a fine-grained PAT first:

1. GitHub → Settings → Developer settings → Personal access tokens →
   Fine-grained tokens → Generate new token
2. Resource owner: `Fpgades`
3. Repository access: Only select repositories → `x-ui_Modified`
4. Permissions → Repository permissions → **Contents: Read and write**
5. Generate, copy the `github_pat_...` value

Then on the server:

```bash
mkdir -p /opt/xui-mesh && cd /opt/xui-mesh

# Store credentials securely (NOT in URL, NOT in shell history).
cat > ~/.git-credentials <<EOF
https://Fpgades:PASTE_TOKEN_HERE@github.com
EOF
chmod 600 ~/.git-credentials
git config --global credential.helper store

# Clone
git clone https://github.com/Fpgades/x-ui_Modified.git src
cd src
git config user.email "dev@xui-mesh.local"
git config user.name  "xui-mesh dev"
git log --oneline -5
```

## Step 4 — first build (baseline sanity check)

The first build will be slow (5-10 min) because Go fetches all deps and
the toolchain. Use tmux so an OOM-driven SSH disconnect doesn't kill it.

```bash
tmux new -s build
# inside tmux:
cd /opt/xui-mesh/src
GOFLAGS="-p=2" GOMEMLIMIT=1500MiB CGO_ENABLED=1 \
  go build -o /tmp/x-ui-baseline -trimpath -ldflags="-s -w" main.go 2>&1 | tee /tmp/build.log
echo "EXIT: $?"
ls -lh /tmp/x-ui-baseline
file /tmp/x-ui-baseline
# detach: Ctrl+B then D
```

If it succeeds — you have a working dev environment. The binary at
`/tmp/x-ui-baseline` is upstream 3x-ui, unmodified — you can run it
(`/tmp/x-ui-baseline` then point browser at port 2053) to confirm.

## Step 5 — generate gRPC code

When `mesh/proto/mesh.proto` exists in the tree (it does, after this
PR), run:

```bash
cd /opt/xui-mesh/src
make proto
```

This regenerates `mesh/pb/*.pb.go` from `.proto`. We commit the
generated files so nodes/users don't need protoc just to build.

## Step 6 — develop loop

```bash
cd /opt/xui-mesh/src
git pull                    # grab latest changes pushed from Windows
make build                  # ./bin/x-ui (or wherever Makefile puts it)
./bin/x-ui                  # run it
```

For panel mode toggling during dev, see `docs/MESH_DESIGN.md` §3.

## Disk-saving notes

If disk gets tight:

- Go module cache lives in `/root/go/pkg/mod` (~750MB after first build).
  Safe to delete with `go clean -modcache` — will re-download on next
  build.
- Build artefact cache `/root/.cache/go-build` (~600MB). Safe to delete
  with `go clean -cache` — will rebuild incrementally.
- Failed-build temp dirs: `rm -rf /tmp/go-build*`.

## Rollback / wipe

To remove everything xui-mesh-related from the box:

```bash
rm -rf /opt/xui-mesh
go clean -cache -modcache 2>/dev/null
rm -rf /usr/local/go
rm -f /usr/local/bin/go /usr/local/bin/gofmt
rm -f /usr/local/bin/protoc-gen-go /usr/local/bin/protoc-gen-go-grpc
# DO NOT wipe /swapfile or apt packages — they're not specific to us.
```
