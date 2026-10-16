VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COMPOSE := docker compose -f deploy/test/compose.yaml

.PHONY: build test lint it it-up it-down it-reset record demo-data bench clean

build: ## Build spoor and spoorgen into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/spoor ./cmd/spoor
	go build -ldflags "$(LDFLAGS)" -o bin/spoorgen ./cmd/spoorgen

test: ## Unit, golden and MCP protocol tests (no Docker needed)
	go test -race ./...

lint: ## Static checks
	go vet ./...
	gofmt -l . | (! grep .)

it-up: ## Start empty VictoriaMetrics, VictoriaLogs and VictoriaTraces
	$(COMPOSE) up -d --wait

it-down: ## Stop the test backends and delete their data
	$(COMPOSE) down -v

it-reset: it-down it-up ## Restart the test backends with no data

it: ## Integration + scenario tests against real backends (needs `make it-reset` first)
	go test -tags integration -count=1 -v ./test/integration/

record: it-reset ## Re-record golden fixtures from fresh backends, then refresh golden files
	SPOOR_RECORD=1 go test -tags integration -count=1 ./test/integration/
	go test ./internal/digest/ -run TestGolden -update

demo-data: ## Load all six fault scenarios into running backends
	go run ./cmd/spoorgen --scenario all --truth-dir scenarios/out

bench: build ## Run the agent benchmark (see bench/README.md)
	./bench/run.sh

clean:
	rm -rf bin dist scenarios/out
