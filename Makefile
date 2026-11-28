SHELL := /bin/bash

# Load the root .env for compose interpolation when present (it is gitignored).
COMPOSE_ENV := $(if $(wildcard .env),--env-file .env,)

.PHONY: proto build test test-go test-py test-agents test-rag test-eval vet fmt web-install web-dev web-build \
	run-dev-all run-gateway run-workflow run-auth-service run-user-service up down \
	eval-rag eval-rag-chat judge-rag analyze-feedback seed-demo e2e-demo

proto:
	protoc -I proto \
		--go_out=backend/pb --go_opt=paths=source_relative \
		--go-grpc_out=backend/pb --go-grpc_opt=paths=source_relative \
		proto/vectorization/v1/*.proto

build:
	cd backend && go build ./...

test: test-go test-py test-agents test-eval

test-go:
	cd backend && go test ./...

test-py:
	cd services/llamaindex && python3 -m pytest -q

test-agents:
	cd services/agent-hub && python3 -m pytest -q

# Backwards-compatible alias for the old RAG-only test target.
test-rag: test-agents

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

run-auth-service:
	cd backend && go run ./cmd/auth-service

run-user-service:
	cd backend && go run ./cmd/user-service

up:
	docker compose $(COMPOSE_ENV) -f deploy/docker-compose.yml up --build

down:
	docker compose $(COMPOSE_ENV) -f deploy/docker-compose.yml down -v

# Import demo knowledge, accounts and an API key (requires a running stack).
seed-demo:
	python3 scripts/seed_demo_data.py

# Walk one document through the whole pipeline and refresh the evidence report.
e2e-demo:
	python3 scripts/e2e_demo.py --output docs/e2e-evidence.md

# Offline RAG evaluation (requires a running gateway + query service).
eval-rag:
	python3 eval/rag_eval.py --base-url http://127.0.0.1:8080 --k 2

eval-rag-chat:
	python3 eval/rag_eval.py --base-url http://127.0.0.1:8080 --k 2 --with-chat

# LLM-as-judge faithfulness/relevancy/completeness (requires RAG_LLM_* config).
judge-rag:
	python3 eval/judge.py --base-url http://127.0.0.1:8080 --k 2

# Attribute negative feedback and export a replayable regression set.
analyze-feedback:
	python3 eval/analyze_feedback.py --base-url http://127.0.0.1:8080 --limit 200

# Unit tests for the offline eval scripts (stdlib unittest, no deps).
test-eval:
	python3 -m unittest discover -s eval -p 'test_*.py' -v
