# 2SCloud Edge Gateway

The edge gateway is the **single external entrypoint** to the 2SCloud private cloud. All inbound traffic passes through it before reaching any backend service. It enforces security policies via hot-reloadable WebAssembly modules written in Rust.

```
Internet
  └─► Edge Gateway (Go/Fiber)
        ├─ WAF module (Rust/WASM)       — blocks malicious requests
        ├─ Rate-limit module (Rust/WASM) — per-IP quotas
        └─► Backend services (scloud-compute namespace)
```

## Architecture

| Component | Role |
|---|---|
| `scloud-edge-gateway` | This repo — Go/Fiber HTTP gateway |
| `scloud-eg-waf` | Rust WAF WASM module |
| `scloud-eg-rate-limit` | Rust rate-limiting WASM module |
| `scloud-eg-firewall` | Rust firewall WASM module (WIP) |
| `scloud-eg-compute` | Kubernetes platform manifests |
| `scloud-dns` | Internal DNS server |
| `scloud-observability` | Grafana / Loki / Tempo stack |

The Kubernetes manifests for the entire platform live in the `scloud-eg-compute` submodule:

```
scloud-eg-compute/
├── platform/     — scloud-compute namespace (backend workloads)
│   ├── rbac/
│   ├── governance/
│   ├── workloads/
│   └── network/  — NetworkPolicies (deny-all + allow from gateway)
├── gateway/      — scloud-gateway namespace (edge gateway)
│   ├── rbac/
│   ├── governance/
│   ├── config/   — config.toml + waf.json (ConfigMap)
│   └── workloads/
└── dns/          — scloud-dns namespace (internal DNS)
    ├── rbac/
    ├── config/   — config.json + scloud.internal zone (ConfigMap)
    ├── workloads/
    ├── network/
    └── coredns/  — CoreDNS patch for scloud.internal stub zone
```

---

## Prerequisites

- [k3s](https://k3s.io) or any Kubernetes cluster (v1.21+)
- Docker
- Go 1.24+
- Rust + `wasm32-wasip1` target

```bash
# Install k3s
curl -sfL https://get.k3s.io | sh -

# Install Rust
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
rustup target add wasm32-wasip1

# Pull all submodules (WASM modules + platform manifests)
git submodule update --init --recursive
```

---

## Deploying the full cloud

### 1. Build the Docker image

The image compiles all Rust WASM modules and the Go binary in a single multi-stage build.

```bash
make docker-build
```

### 2. Load the image into k3s

k3s uses containerd, not the Docker daemon — the image must be imported explicitly.

```bash
make docker-load
```

### 3. Deploy everything

Applies all manifests in the correct order: namespaces → RBAC → configs → workloads → NetworkPolicies → gateway → CoreDNS patch.

```bash
make k8s-up
```

That's it. The gateway is now the only external entrypoint and `*.scloud.internal` DNS is live.

---

## Verify it works

```bash
# Get the gateway's external IP
kubectl get svc edge-gateway -n scloud-gateway

# Health check
curl http://<EXTERNAL-IP>/healthz
# → ok

# WAF blocking path traversal
curl http://<EXTERNAL-IP>/../etc/passwd
# → 403 Forbidden

# Internal DNS from inside the cluster
kubectl run -it --rm dns-test --image=busybox --restart=Never -- \
  nslookup gateway.scloud.internal
# → CNAME edge-gateway.scloud-gateway.svc.cluster.local
```

---

## Day-to-day operations

| Command | Description |
|---|---|
| `make k8s-status` | Show pods and services across all namespaces |
| `make k8s-reload` | Re-apply configs and restart pods after a config change |
| `make k8s-deploy` | Full cycle: rebuild image → load → redeploy |
| `make k8s-down` | Delete all 2scloud namespaces |
| `make start` | Run the gateway locally without Docker |
| `make buildnstart` | Compile WASM modules + run locally |

---

## Configuration

### WAF rules (`scloud-eg-compute/gateway/config/configmap.yaml`)

Rules are defined in `waf.json` inside the ConfigMap. They are hot-reloaded every 60 seconds — no pod restart needed.

```json
{
  "id": "my-rule",
  "type": "regex",
  "values": ["(?i)evil-pattern"],
  "action": "block"
}
```

After editing the ConfigMap:
```bash
kubectl apply -f scloud-eg-compute/gateway/config/configmap.yaml
# No restart needed — hot-reload picks it up within 60s
```

### Internal DNS (`scloud-eg-compute/dns/config/configmap.yaml`)

Add a new internal service to `scloud.internal`:

```zone
myservice  IN CNAME  myservice.mynamespace.svc.cluster.local.
```

Then:
```bash
kubectl apply -f scloud-eg-compute/dns/config/configmap.yaml
# scloud-dns hot-reloads the zone automatically
```

---

## Local development

```bash
# Run without Docker or Kubernetes
make buildnstart
```

The gateway starts on `http://localhost:8080`. WAF rules are loaded from `internal/config/rules/waf.json`.

---

## Project structure

```
cmd/edge-gateway/       — entrypoint
internal/
  config/               — config structs + config.toml + WAF rules
  runtime/              — WASM module lifecycle (load, init, call)
  server/               — Fiber HTTP server setup
  utils/                — logging, colors
modules/                — Rust WASM security modules (git submodules)
scloud-eg-compute/      — Kubernetes platform manifests (git submodule)
Dockerfile              — multi-stage build (Rust WASM + Go binary)
Makefile                — build, Docker, Kubernetes targets
```

---

## Roadmap v1.0.0

- [x] WAF module (block / log / disabled modes)
- [x] Rate-limit module
- [x] Edge gateway as single Kubernetes entrypoint
- [x] Internal DNS (`scloud.internal`)
- [ ] Firewall module (L3/L4/L7)
- [ ] TLS / HTTPS on the gateway
- [ ] Authentication module (mTLS, token validation)
- [ ] Authorization module (JWT, OAuth scopes)
- [ ] Bot detection
- [ ] DDoS heuristics
- [ ] Frontend dashboard
- [ ] Observability (Prometheus + Grafana + Loki)
