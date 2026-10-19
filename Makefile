# parallax-mcp build targets.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ParallaxProtocol/parallax-mcp/pkg/server.Version=$(VERSION)
IMAGE   ?= ghcr.io/parallaxprotocol/parallax-mcp

.PHONY: all build tools test race live live-full lint fmt docker run-stdio run-http sync-content clean

all: lint test build

build: ## Build bin/parallax-mcp (static, no cgo)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/parallax-mcp ./cmd/parallax-mcp

tools: ## Build bin/mcpcall, the command-line MCP client for trying the server
	go build -o bin/mcpcall ./cmd/mcpcall

test: ## Unit tests (fake backends, no network)
	go test ./...

race: ## Unit tests with the race detector
	go test -race ./...

live: ## Integration tests against the real endpoints
	PARALLAX_MCP_LIVE=1 go test -count=1 -v ./test/integration/

live-full: ## Integration tests against a local node with all namespaces (RPC=http://127.0.0.1:8545)
	PARALLAX_MCP_LIVE=1 PARALLAX_MCP_LIVE_RPC=$(or $(RPC),http://127.0.0.1:8545) go test -count=1 -v ./test/integration/

lint: ## go vet and a gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }

fmt:
	gofmt -w .

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

run-stdio: build
	./bin/parallax-mcp --log-format text

run-http: build
	./bin/parallax-mcp --transport http --log-format text

sync-content: ## Refresh the embedded docs and PIPs (needs ../parallax-docs and the gh CLI)
	scripts/sync-content.sh

clean:
	rm -rf bin
