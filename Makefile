SHELL := /bin/bash

.PHONY: proto build test test-go test-py vet fmt web-install web-dev web-build \
	run-dev-all run-gateway run-workflow up down

proto:
	protoc -I proto \
		--go_out=backend/pb --go_opt=paths=source_relative \
		--go-grpc_out=backend/pb --go-grpc_opt=paths=source_relative \
		proto/vectorization/v1/*.proto

build:
	cd backend && go build ./...

test: test-go test-py

test-go:
	cd backend && go test ./...

test-py:
	cd services/llamaindex && python3 -m pytest -q

vet:
	cd backend && go vet ./...

fmt:
	cd backend && gofmt -w cmd internal

web-install:
	cd web && npm install

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm install && npm run build

run-dev-all:
	cd backend && go run ./cmd/dev-all

run-gateway:
	cd backend && go run ./cmd/gateway

run-workflow:
	cd backend && go run ./cmd/workflow-engine

up:
	docker compose -f deploy/docker-compose.yml up --build

down:
	docker compose -f deploy/docker-compose.yml down -v
