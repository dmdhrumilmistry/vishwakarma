# syntax=docker/dockerfile:1.7
# Multi-stage build: a static binary on a distroless, non-root base.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/vishwakarma ./cmd/vishwakarma

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Vishwakarma" \
      org.opencontainers.image.description="Self-hosted throwaway VMs and containers on Kubernetes for testing apps and endpoint tools" \
      org.opencontainers.image.source="https://github.com/dmdhrumilmistry/vishwakarma" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/vishwakarma /usr/local/bin/vishwakarma
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/vishwakarma"]
CMD ["serve"]
