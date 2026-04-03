FROM rust:1.86-slim AS wasm-builder

RUN rustup target add wasm32-wasip1

WORKDIR /build

COPY modules/scloud-eg-waf        modules/scloud-eg-waf
COPY modules/scloud-eg-rate-limit modules/scloud-eg-rate-limit
COPY modules/scloud-eg-firewall   modules/scloud-eg-firewall

RUN cd modules/scloud-eg-waf        && cargo build --release --target wasm32-wasip1
RUN cd modules/scloud-eg-rate-limit && cargo build --release --target wasm32-wasip1
RUN cd modules/scloud-eg-firewall   && cargo build --release --target wasm32-wasip1

FROM golang:1.25-alpine AS go-builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/       cmd/
COPY internal/  internal/

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o edge-gateway ./cmd/edge-gateway

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Binary
COPY --from=go-builder /build/edge-gateway .

# WASM modules — paths must match config.toml in the ConfigMap:
#   /app/modules/scloud-eg-waf/.../scloud_eg_waf.wasm
#   /app/modules/scloud-eg-rate-limit/.../scloud_eg_rate_limit.wasm
COPY --from=wasm-builder \
  /build/modules/scloud-eg-waf/target/wasm32-wasip1/release/scloud_eg_waf.wasm \
  modules/scloud-eg-waf/target/wasm32-wasip1/release/scloud_eg_waf.wasm

COPY --from=wasm-builder \
  /build/modules/scloud-eg-rate-limit/target/wasm32-wasip1/release/scloud_eg_rate_limit.wasm \
  modules/scloud-eg-rate-limit/target/wasm32-wasip1/release/scloud_eg_rate_limit.wasm

# The config and WAF rules are mounted by Kubernetes via ConfigMap —
# no need to bake them into the image.

EXPOSE 8080

ENTRYPOINT ["/app/edge-gateway"]
