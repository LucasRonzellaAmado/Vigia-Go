SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DATABASE_URL ?= postgres://vigia:vigia@localhost:5432/vigia?sslmode=disable
SQLC ?= $(shell command -v sqlc 2>/dev/null || echo docker run --rm -v $(CURDIR):/src -w /src sqlc/sqlc:1.31.1)
GOOSE ?= go run github.com/pressly/goose/v3/cmd/goose@v3.28.0

export DATABASE_URL

.PHONY: help run build test test-race test-integration cover vet fmt sqlc sqlc-check \
        migrate-up migrate-down migrate-status migrate-new db-up up down logs docker-build mem clean

help:
	@grep -E '^[a-zA-Z_-]+:' Makefile | grep -v '^help' | cut -d: -f1 | sort | xargs -n1 echo "  make"

run: db-up
	VIGIA_LOG_FORMAT=text go run ./cmd/vigia serve

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/vigia ./cmd/vigia

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -race -tags integration -count=1 ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

vet:
	go vet -tags integration ./...

fmt:
	gofmt -w cmd internal

sqlc:
	$(SQLC) generate

sqlc-check:
	$(SQLC) diff

migrate-up:
	go run ./cmd/vigia migrate up

migrate-down:
	go run ./cmd/vigia migrate down

migrate-status:
	go run ./cmd/vigia migrate status

migrate-new:
	@test -n "$(name)" || (echo "uso: make migrate-new name=add_coisa" && exit 1)
	$(GOOSE) -dir internal/store/migrations create $(name) sql

db-up:
	docker compose up -d postgres

up:
	VERSION=$(VERSION) docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f vigia

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t vigia:$(VERSION) .

mem:
	@docker stats --no-stream --format '{{.Name}}: {{.MemUsage}} (CPU {{.CPUPerc}})' vigia-vigia-1 vigia-postgres-1

clean:
	rm -rf bin coverage.out
