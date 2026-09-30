.PHONY: fmt fmt-check vet test test-integration check build run migrate db-up db-down

COMPOSE := docker compose -f deployments/docker-compose.yml

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "$$out"; \
		echo "gofmt needed"; \
		exit 1; \
	fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

# Requires a reachable PostgreSQL configured through MADICLOUD_DB_*.
test-integration:
	MADICLOUD_INTEGRATION=1 go test -race -count=1 -run Integration ./...

check: fmt-check vet test

build:
	mkdir -p bin
	go build -o bin/madicloudd ./cmd/madicloudd
	go build -o bin/madicloud ./cmd/madicloud

run: build
	./bin/madicloudd serve

migrate: build
	./bin/madicloudd migrate

db-up:
	$(COMPOSE) up -d --wait

db-down:
	$(COMPOSE) down
