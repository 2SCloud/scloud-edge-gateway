# SCloud Edge Gateway
## Project struct explained:
- `cmd/edge-gateway/main.go` -> Entrypoint

- `internal/config` -> All logic concerning configuration + configuration files
- `internal/config/rules` -> JSON configurations files (hot-reloadable)
- `internal/runtime/wasm.go` -> Logic for calling Rust WASM compilated files
- `internal/utils` -> Utilities (logs, colors...etc)

- `modules/` -> Rust security modules
- `modules/waf` -> Web Application Firewall
- `modules/firewall` -> L3/L4/L7 filtering

### Later:
- `modules/ratelimit` -> Rate limiting / quotas
- `modules/bot-detection` -> Bot / scraper detection
- `modules/ddos` -> DDoS heuristics
- `modules/authz` -> AuthZ (JWT, OAuth scopes)
- `modules/authn` -> AuthN (mTLS, tokens)
- `modules/ip-reputation` -> IP reputation / geo / ASN
- `modules/header-sanitizer` -> IP reputation / geo / ASN
- `modules/request-validator` -> Schema / contract validation
- `modules/response-filter` -> Response masking / DLP
- `modules/anomaly-detection` -> Behavioral anomalies
- `modules/circuit-breaker` -> Backend protection
- `modules/shadow-mode` -> Observe-only security
- `modules/sandbox` -> Untrusted plugins

- `deploy/docker/Dockerfile` -> Deploy on docker
- `deploy/kubernetes/edge-gateway.yaml` -> Deploy on kubernetes


## Deployment
Compile each Rust security modules:
```bash
cd modules/scloud-eg-{module-name}
rustup target add wasm32-wasip1
cargo build --release --target wasm32-wasip1
```

After that you need to launch the gateway:
```bash
go run cmd/edge-gateway/main.go
```

## Roadmap v1.0.0
- [ ] Modules are ajustable (init - inline(hot-reload))
  - [ ] Only JSON "sub-config" are hot-reloadable
  - [ ] TOML config is static
- [ ] Security modules:
  - [ ] WAF
  - [ ] rate-limit
  - [ ] firewall
- [ ] The Edge-Gateway is the only entrypoint
- [ ] Main dashboard with traffic datas
  - [ ] Observability OK
    - [ ] Prometheus
    - [ ] Grafana
