# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/ParallaxProtocol/parallax-mcp/internal/server.Version=${VERSION}" \
    -o /out/parallax-mcp ./cmd/parallax-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/parallax-mcp /usr/local/bin/parallax-mcp
USER nonroot:nonroot
EXPOSE 8080
ENV PARALLAX_MCP_TRANSPORT=http \
    PARALLAX_MCP_HTTP_ADDR=0.0.0.0:8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/parallax-mcp", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/parallax-mcp"]
