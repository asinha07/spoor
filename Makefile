VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
COMPOSE := docker compose -f deploy/test/compose.yaml
GRAFANA_COMPOSE := docker compose -f deploy/test/grafana-compose.yaml

.PHONY: build test lint it it-up it-down it-reset it-grafana it-grafana-up it-grafana-down record demo-data messy-data bench clean

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

it-grafana-up: ## Start empty Prometheus, Loki and Tempo (deletes their data)
	$(GRAFANA_COMPOSE) down -v
	$(GRAFANA_COMPOSE) up -d
	@echo "waiting for Loki and Tempo to become ready..."; for i in $$(seq 1 60); do \
	  curl -sf localhost:3100/ready >/dev/null && curl -sf localhost:3200/ready >/dev/null && curl -sf localhost:9090/-/ready >/dev/null && exit 0; sleep 2; done; exit 1

it-grafana-down: ## Stop Prometheus, Loki and Tempo and delete their data
	$(GRAFANA_COMPOSE) down -v

it-grafana: it-grafana-up ## Scenario tests against real Prometheus, Loki and Tempo
	go test -tags integration,grafana -count=1 -v -run TestGrafanaStack ./test/integration/

record: it-reset ## Re-record golden fixtures from fresh backends, then refresh golden files
	SPOOR_RECORD=1 go test -tags integration -count=1 ./test/integration/
	go test ./internal/digest/ -run TestGolden -update

demo-data: ## Load all six fault scenarios into running backends
	go run ./cmd/spoorgen --scenario all --truth-dir scenarios/out

messy-data: ## Load the production-shaped messy-logs scenario (use with deploy/test/messy.yaml)
	go run ./cmd/spoorgen --scenario messy-logs --end $$(date -u -d '-40 hours' +%Y-%m-%dT%H:00:00Z 2>/dev/null || date -u -v-40H +%Y-%m-%dT%H:00:00Z) --truth-dir scenarios/out

bench: build ## Run the agent benchmark (see bench/README.md)
	./bench/run.sh

clean:
	rm -rf bin dist scenarios/out
