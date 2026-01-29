build:

    @echo "Building scloud-eg-firewall..."
    cd modules/scloud-eg-firewall
    rustup target add wasm32-wasip1
    cargo build --release --target wasm32-wasip1

    @echo "Building scloud-eg-waf..."
	cd ../scloud-eg-waf
	rustup target add wasm32-wasip1
    cargo build --release --target wasm32-wasip1

	@echo "Building scloud-eg-rate-limit..."
	cd ../scloud-eg-rate-limit
	rustup target add wasm32-wasip1
    cargo build --release --target wasm32-wasip1

	@echo "Starting the Edge Gateway..."
	cd ../..
	go run cmd/edge-gateway/main.go

# TODO: add build for scloud-eg-compute when ready

clean:
	@docker compose down --rmi all -v --remove-orphans

start:
	@docker compose up -d