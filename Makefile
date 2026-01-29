.ONESHELL:
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

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

# TODO: add build for scloud-eg-compute when ready

clean:
	@echo "Cleaning build artifacts..."
	cd modules/scloud-eg-firewall && cargo clean
	cd ../scloud-eg-waf && cargo clean
	cd ../scloud-eg-rate-limit && cargo clean
	go clean -modcache

start:
	@echo "Starting the Edge Gateway..."
	go run cmd/edge-gateway/main.go
