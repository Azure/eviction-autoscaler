# syntax=docker/dockerfile:1.6

# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

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

# Build for the target architecture
RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
FROM --platform=$TARGETPLATFORM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]