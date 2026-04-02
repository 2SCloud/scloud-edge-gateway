.ONESHELL:
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

IMAGE     ?= 2scloud/edge-gateway
TAG       ?= latest
COMPUTE   := scloud-eg-compute

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

# Load the image into a local k3s cluster (k3s uses containerd, not Docker daemon)
docker-load:
	@echo "Loading $(IMAGE):$(TAG) into k3s containerd..."
	docker save $(IMAGE):$(TAG) | k3s ctr images import -

# Build + load in one step
docker: docker-build docker-load

# ── Kubernetes ────────────────────────────────────────────────────────────────

# Deploy the full cluster (first time)
k8s-up:
	@echo "==> [1/5] Namespaces"
	kubectl apply -f $(COMPUTE)/platform/namespace.yaml
	kubectl apply -f $(COMPUTE)/gateway/namespace.yaml
	kubectl apply -f $(COMPUTE)/dns/namespace.yaml

	@echo "==> [2/5] RBAC & governance"
	kubectl apply -f $(COMPUTE)/platform/rbac/
	kubectl apply -f $(COMPUTE)/platform/governance/
	kubectl apply -f $(COMPUTE)/gateway/rbac/
	kubectl apply -f $(COMPUTE)/gateway/governance/
	kubectl apply -f $(COMPUTE)/dns/rbac/

	@echo "==> [3/5] ConfigMaps"
	kubectl apply -f $(COMPUTE)/gateway/config/
	kubectl apply -f $(COMPUTE)/dns/config/

	@echo "==> [4/5] Workloads & NetworkPolicies (compute locked down before gateway is live)"
	kubectl apply -f $(COMPUTE)/platform/workloads/
	kubectl apply -f $(COMPUTE)/platform/network/
	kubectl apply -f $(COMPUTE)/dns/workloads/
	kubectl apply -f $(COMPUTE)/dns/network/

	@echo "==> [5/5] Gateway (last — compute is already locked)"
	kubectl apply -f $(COMPUTE)/gateway/workloads/

	@echo "==> Patching CoreDNS for scloud.internal..."
	kubectl apply -f $(COMPUTE)/dns/coredns/coredns-patch.yaml
	kubectl rollout restart deployment/coredns -n kube-system

	@echo ""
	@echo "Done. Waiting for gateway to be ready..."
	kubectl rollout status deployment/edge-gateway -n scloud-gateway

# Tear everything down (keeps the cluster, removes 2scloud namespaces)
k8s-down:
	kubectl delete namespace scloud-gateway scloud-compute scloud-dns --ignore-not-found

# Re-apply configs + restart pods (after a config change)
k8s-reload:
	kubectl apply -f $(COMPUTE)/gateway/config/
	kubectl apply -f $(COMPUTE)/dns/config/
	kubectl rollout restart deployment/edge-gateway -n scloud-gateway
	kubectl rollout restart deployment/scloud-dns    -n scloud-dns

# Show status of all 2scloud components
k8s-status:
	@echo "── Gateway ──────────────────────────────"
	kubectl get pods,svc -n scloud-gateway
	@echo "── Compute ──────────────────────────────"
	kubectl get pods,svc -n scloud-compute
	@echo "── DNS ──────────────────────────────────"
	kubectl get pods,svc -n scloud-dns

# Full rebuild: build image, load it, redeploy
k8s-deploy: docker k8s-reload
