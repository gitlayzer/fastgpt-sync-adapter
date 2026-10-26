# Multi-stage build for fastgpt-sync-adapter
# Supports both amd64 and arm64 with cgo support for WeCom C-SDK.

FROM golang:1.25-bookworm AS builder

WORKDIR /workspace

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc g++ libc6-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Target architecture passed by Docker buildx
ARG TARGETARCH
ARG VERSION=v2.0.0
ARG GIT_COMMIT=none
ARG BUILD_DATE=unknown

# Build adapter binary
RUN CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags "-w -s \
      -X 'github.com/gitlayzer/fastgpt-sync-adapter/internal/version.Version=${VERSION}' \
	      -X 'github.com/gitlayzer/fastgpt-sync-adapter/internal/version.GitCommit=${GIT_COMMIT}' \
	      -X 'github.com/gitlayzer/fastgpt-sync-adapter/internal/version.BuildDate=${BUILD_DATE}'" \
	    -o /workspace/bin/adapter ./cmd/adapter

# Runtime Stage
FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libstdc++6 \
    tzdata \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Copy binaries
COPY --from=builder /workspace/bin/adapter /app/bin/adapter

# Copy WeCom dynamic libraries and default configs
COPY lib/ /app/lib/
COPY config.example.yaml /app/config.example.yaml
COPY identity_map.example.json /app/identity_map.example.json

# Setup library paths for both architectures
ENV LD_LIBRARY_PATH=/app/lib/arm64:/app/lib/amd64
ENV PATH="/app/bin:${PATH}"

# Expose HTTP ports:
# 8080: FastGPT API 文件库模式 (serve)
# 8089: 飞书 OAuth 授权回调与 Webhook
EXPOSE 8080 8089

ENTRYPOINT ["adapter"]
CMD ["--config", "/app/config.yaml"]
