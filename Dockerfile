# syntax=docker/dockerfile:1.6

# Build the manager binary
# Use Microsoft's FIPS-validated Go distribution (includes BoringCrypto)
FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.25 AS builder

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

# Build for the target architecture with FIPS-validated BoringCrypto (requires CGO)
RUN CGO_ENABLED=1 \
    GOEXPERIMENT=boringcrypto \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -a -o manager cmd/main.go

# Use Azure Linux distroless as FIPS-compliant minimal base image
FROM --platform=$TARGETPLATFORM mcr.microsoft.com/azurelinux/distroless/minimal:3.0
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]