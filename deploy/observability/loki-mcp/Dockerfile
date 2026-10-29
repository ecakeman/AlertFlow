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
