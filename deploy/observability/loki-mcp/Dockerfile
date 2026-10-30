# Loki MCP image (grafana/loki-mcp v0.6.0), built from this repo.
#
# This is the only build context for the Loki MCP image: the docker-compose
# `observability` profile, the CI "build Loki MCP" job and the Kind import all
# use this path. Do not bump the upstream tag: the Agent's LOKI_MCP_URL and the
# `loki_query` tool schema are pinned to v0.6.0 and the `/stream` endpoint
# (see docs/v2/CONTRACT.md).
FROM golang:1.24.4 AS build
WORKDIR /src
RUN git clone --depth 1 --branch v0.6.0 https://github.com/grafana/loki-mcp.git .
ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
RUN go build -o /out/loki-mcp-server ./cmd/server

FROM debian:bookworm-slim
COPY --from=build /out/loki-mcp-server /loki-mcp-server
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/loki-mcp-server"]
