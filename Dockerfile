# syntax=docker/dockerfile:1.6

# Build the manager binary
# Use Microsoft's FIPS-validated Go distribution (uses OpenSSL backend for FIPS)
# Use --platform=$TARGETPLATFORM so CGO compiles natively via QEMU, avoiding
# cross-compiler issues with CGO_ENABLED=1 on arm64.
FROM --platform=$TARGETPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.25 AS builder

WORKDIR /workspace

# BuildKit args (populated by docker buildx)
ARG TARGETOS
ARG TARGETARCH

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build for the target architecture.
# Microsoft Go routes crypto through OpenSSL (FIPS-validated) when CGO_ENABLED=1 on Linux.
# GOEXPERIMENT=boringcrypto is upstream-only and not used here.
RUN CGO_ENABLED=1 \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -a -o manager cmd/main.go

# Use Azure Linux distroless base as FIPS-compliant runtime image.
# 'base' variant includes glibc and libssl required by the CGO/OpenSSL-linked binary.
FROM --platform=$TARGETPLATFORM mcr.microsoft.com/azurelinux/distroless/base:3.0
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]