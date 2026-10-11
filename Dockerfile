# syntax=docker/dockerfile:1

# Official tags are the defaults. Override NODE_IMAGE, GO_IMAGE, or
# RUNTIME_IMAGE when a registry is unreachable. NPM_REGISTRY and GOPROXY
# are optional and stay empty unless a package download itself fails.
ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.25
ARG RUNTIME_IMAGE=gcr.io/distroless/static:latest

# --- Stage 1: build the Vue single-page app ---
# Runs natively on the build platform: the output is plain JS/HTML/CSS with
# no arch dependency, so building it once here avoids QEMU-emulating npm for
# every target platform (emulated npm ci is extremely slow).
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web
ARG NPM_REGISTRY=
WORKDIR /web
COPY web-ui/package.json web-ui/package-lock.json ./
RUN if [ -n "$NPM_REGISTRY" ]; then npm config set registry "$NPM_REGISTRY"; fi && npm ci
COPY web-ui/ ./
RUN npm run build

# --- Stage 2: build the static Go binary (embeds the SPA) ---
# Runs natively on the build platform and cross-compiles to the target arch.
# The PostgreSQL driver is pure Go, so CGO stays disabled.
ARG GO_IMAGE=golang:1.25
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
# TARGETOS/TARGETARCH are injected by BuildKit from the target platform
# (the host's arch for a plain `docker build`, or each --platform under buildx).
ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY=
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN if [ -n "$GOPROXY" ]; then go env -w GOPROXY="$GOPROXY"; fi && go mod download
COPY backend/ ./
# Embed the freshly built SPA into the binary.
COPY --from=web /web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /alertflow ./cmd/alertflow

# --- Stage 3: minimal runtime image ---
ARG RUNTIME_IMAGE=gcr.io/distroless/static:latest
FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.title="AlertFlow" \
      org.opencontainers.image.description="Event-driven service alerts, based on upmonitor" \
      org.opencontainers.image.source="https://github.com/ecakeman/AlertFlow"
COPY --from=build /alertflow /alertflow
ENV ALERTFLOW_CONFIG_DIR=/config \
    ALERTFLOW_ADDR=:8080
EXPOSE 8080
VOLUME ["/config"]
ENTRYPOINT ["/alertflow"]
