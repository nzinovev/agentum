# The published Postgres port is overridable (5432 is often already taken) and
# exported, so `AGENTUM_PG_PORT=55432 make test-db` moves compose and the DSN
# together.
AGENTUM_PG_PORT ?= 5432
export AGENTUM_PG_PORT

PG_URL ?= postgres://agentum:agentum@localhost:$(AGENTUM_PG_PORT)/agentum?sslmode=disable&search_path=agentum

# Foreground run vs background run:
# - `make run` runs in the foreground; stop it with Ctrl+C. Nothing to clean up.
# - `make run-bg` builds a binary into ./bin and runs it detached, writing its
#   PID to PID_FILE and stdout/stderr to LOG_FILE. Stop it with `make stop`.
#   Using a real binary (not `go run`) avoids orphaned children when the parent
#   is killed.
AGENTUM_BIN := bin/agentum
PID_FILE    := /tmp/agentum.pid
LOG_FILE    := /tmp/agentum.log

.PHONY: help tidy build run run-bg stop logs test test-db vet fmt sqlc-gen migrate-up migrate-down docker-up docker-down

help: ## show this help
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*##"}; {printf "  %-16s %s\n", $$1, $$2}'

tidy: ## go mod tidy
	go mod tidy

build: ## build all packages
	go build ./...

SOURCES := $(wildcard cmd/agentum/*.go internal/*/*.go internal/store/migrations/*.sql)

$(AGENTUM_BIN): $(SOURCES)
	@mkdir -p bin
	go build -o $(AGENTUM_BIN) ./cmd/agentum

run: ## run the server in the foreground (needs Postgres; try: make docker-up). Stop with Ctrl+C.
	go run ./cmd/agentum

run-bg: $(AGENTUM_BIN) ## run the server in the background (logs to LOG_FILE, pid to PID_FILE)
	@if [ -f $(PID_FILE) ] && kill -0 $$(cat $(PID_FILE)) 2>/dev/null; then \
		echo "already running (pid $$(cat $(PID_FILE))); use 'make stop' first"; exit 1; \
	fi
	@nohup ./$(AGENTUM_BIN) > $(LOG_FILE) 2>&1 & echo $$! > $(PID_FILE); \
	sleep 1; \
	if kill -0 $$(cat $(PID_FILE)) 2>/dev/null; then \
		echo "started agentum (pid $$(cat $(PID_FILE)))"; \
		echo "  logs: make logs"; \
		echo "  stop: make stop"; \
	else \
		echo "failed to start; check $(LOG_FILE)"; rm -f $(PID_FILE); exit 1; \
	fi

stop: ## stop the background server started by run-bg
	@if [ -f $(PID_FILE) ] && kill -0 $$(cat $(PID_FILE)) 2>/dev/null; then \
		kill $$(cat $(PID_FILE)) && echo "stopped agentum ($$(cat $(PID_FILE)))"; \
	else \
		echo "agentum not running (no live pid at $(PID_FILE))"; \
	fi
	@rm -f $(PID_FILE)

logs: ## tail the background server log (Ctrl+C to exit)
	@tail -n 50 -f $(LOG_FILE)

test: ## run tests
	go test ./...

# Postgres-backed tests skip under a plain `go test` (unset
# AGENTUM_TEST_DATABASE_URL); this target is how they actually run. --wait
# blocks on the compose healthcheck, so tests never race the database boot.
test-db: ## run all tests incl. Postgres-backed ones (starts compose Postgres, waits for health)
	docker compose up -d --wait
	AGENTUM_TEST_DATABASE_URL="$(PG_URL)" go test ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt -s
	gofmt -s -w .

sqlc-gen: ## generate sqlc code (needs sqlc: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest)
	sqlc generate

migrate-up: ## apply migrations via goose CLI (the app also auto-migrates on boot)
	goose -dir internal/store/migrations postgres "$(PG_URL)" up

migrate-down: ## roll back one migration via goose CLI
	goose -dir internal/store/migrations postgres "$(PG_URL)" down

docker-up: ## start Postgres via docker compose
	docker compose up -d

docker-down: ## stop docker compose services
	docker compose down
