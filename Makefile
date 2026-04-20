.ONESHELL:
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

IMAGE          ?= 2scloud/edge-gateway
FRONTEND_IMAGE ?= 2scloud/frontend
TAG            ?= latest
COMPUTE        := scloud-eg-compute

# ── Local dev ─────────────────────────────────────────────────────────────────

buildnstart:
	@echo "Checking for updates in all submodules..."
	git submodule update --remote --recursive

	@echo "Ensuring Rust target wasm32-wasip1 is installed..."
	rustup target add wasm32-wasip1

	@echo "Building scloud-eg-firewall..."
	cd modules/scloud-eg-firewall && cargo build --release --target wasm32-wasip1

	@echo "Building scloud-eg-waf..."
	cd ../scloud-eg-waf && cargo build --release --target wasm32-wasip1

	@echo "Building scloud-eg-rate-limit..."
	cd ../scloud-eg-rate-limit && cargo build --release --target wasm32-wasip1

	@echo "Starting the Edge Gateway..."
	cd ../.. && go run cmd/edge-gateway/main.go

start:
	@echo "Starting the Edge Gateway..."
	go run cmd/edge-gateway/main.go

clean:
	@echo "Cleaning build artifacts..."
	cd modules/scloud-eg-firewall && cargo clean
	cd ../scloud-eg-waf && cargo clean
	cd ../scloud-eg-rate-limit && cargo clean
	go clean -modcache

# ── Docker ────────────────────────────────────────────────────────────────────

# Build the edge gateway Docker image (includes WASM compilation)
docker-build:
	@echo "Building Docker image $(IMAGE):$(TAG)..."
	git submodule update --init --recursive
	docker build -t $(IMAGE):$(TAG) .

# Load the image into a local k3s cluster (k3s uses containerd, not Docker daemon).
# The k3s containerd socket is root-owned (/run/k3s/containerd/containerd.sock),
# so `ctr` requires sudo. Matches what deploy.sh does.
docker-load:
	@echo "Loading $(IMAGE):$(TAG) into k3s containerd..."
	docker save $(IMAGE):$(TAG) | sudo k3s ctr images import -

# Build + load in one step
docker: docker-build docker-load

# ── Frontend Docker ───────────────────────────────────────────────────────────

# NEXT_PUBLIC_API_URL is baked into the client bundle at build time.
# Override with: make frontend-build NEXT_PUBLIC_API_URL=http://your-admin-host:9090
NEXT_PUBLIC_API_URL ?= http://localhost:9090

frontend-build:
	@echo "Building frontend image $(FRONTEND_IMAGE):$(TAG) (NEXT_PUBLIC_API_URL=$(NEXT_PUBLIC_API_URL))..."
	docker build \
		--build-arg NEXT_PUBLIC_API_URL=$(NEXT_PUBLIC_API_URL) \
		-t $(FRONTEND_IMAGE):$(TAG) ./frontend

frontend-load:
	@echo "Loading $(FRONTEND_IMAGE):$(TAG) into k3s containerd..."
	docker save $(FRONTEND_IMAGE):$(TAG) | sudo k3s ctr images import -

frontend: frontend-build frontend-load

# ── Kubernetes ────────────────────────────────────────────────────────────────

# Deploy the full cluster (first time)
k8s-up:
	@echo "==> [1/6] Namespaces"
	kubectl apply -f $(COMPUTE)/platform/namespace.yaml
	kubectl apply -f $(COMPUTE)/gateway/namespace.yaml
	kubectl apply -f $(COMPUTE)/dns/namespace.yaml
	kubectl apply -f $(COMPUTE)/frontend/namespace.yaml

	@echo "==> [2/6] RBAC & governance"
	kubectl apply -f $(COMPUTE)/platform/rbac/
	kubectl apply -f $(COMPUTE)/platform/governance/
	kubectl apply -f $(COMPUTE)/gateway/rbac/
	kubectl apply -f $(COMPUTE)/gateway/governance/
	kubectl apply -f $(COMPUTE)/dns/rbac/

	@echo "==> [3/6] ConfigMaps"
	kubectl apply -f $(COMPUTE)/gateway/config/
	kubectl apply -f $(COMPUTE)/dns/config/
	kubectl apply -f $(COMPUTE)/frontend/config/

	@echo "==> [4/6] Workloads & NetworkPolicies (compute locked down before gateway is live)"
	kubectl apply -f $(COMPUTE)/platform/workloads/
	kubectl apply -f $(COMPUTE)/platform/network/
	kubectl apply -f $(COMPUTE)/dns/workloads/
	kubectl apply -f $(COMPUTE)/dns/network/
	kubectl apply -f $(COMPUTE)/frontend/workloads/
	kubectl apply -f $(COMPUTE)/frontend/network/

	@echo "==> [5/6] Gateway (last — all backends are already locked)"
	kubectl apply -f $(COMPUTE)/gateway/workloads/

	@echo "==> [6/6] Patching CoreDNS for scloud.internal..."
	kubectl apply -f $(COMPUTE)/dns/coredns/coredns-patch.yaml
	kubectl rollout restart deployment/coredns -n kube-system

	@echo ""
	@echo "Done. Waiting for gateway to be ready..."
	kubectl rollout status deployment/edge-gateway -n scloud-gateway

# Tear everything down (keeps the cluster, removes 2scloud namespaces)
k8s-down:
	kubectl delete namespace scloud-gateway scloud-compute scloud-dns scloud-frontend --ignore-not-found

# Re-apply configs + restart pods (after a config change)
k8s-reload:
	kubectl apply -f $(COMPUTE)/gateway/config/
	kubectl apply -f $(COMPUTE)/dns/config/
	kubectl apply -f $(COMPUTE)/frontend/config/
	kubectl rollout restart deployment/edge-gateway   -n scloud-gateway
	kubectl rollout restart deployment/scloud-dns     -n scloud-dns
	kubectl rollout restart deployment/scloud-frontend -n scloud-frontend

# Show status of all 2scloud components
k8s-status:
	@echo "── Gateway ──────────────────────────────"
	kubectl get pods,svc -n scloud-gateway
	@echo "── Compute ──────────────────────────────"
	kubectl get pods,svc -n scloud-compute
	@echo "── DNS ──────────────────────────────────"
	kubectl get pods,svc -n scloud-dns
	@echo "── Frontend ─────────────────────────────"
	kubectl get pods,svc -n scloud-frontend

# Full rebuild: gateway + frontend images, load them, redeploy
k8s-deploy: docker frontend k8s-reload
