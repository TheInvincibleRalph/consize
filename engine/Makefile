# Consize engine — dev targets
# Binaries: migrate, collector, analyze, costscan, devseed, api. All fall back to an
# in-memory store when DATABASE_URL is unset.

.PHONY: test demo vet build migrate collector analyze costscan devseed api

## test: run the whole suite (policy, store, collector, API, pricing)
test:
	go test ./...

## demo: print the shipped demo report (analysis engine + fixtures)
demo:
	go run ./cmd/demo

## vet: static checks
vet:
	go vet ./...

## build: compile all binaries into bin/
build:
	mkdir -p bin
	go build -o bin/ ./cmd/...

## migrate: apply schema migrations to Postgres (DATABASE_URL required)
migrate:
	go run ./cmd/migrate

## collector: ingest k8s metadata + Prometheus usage (one-shot)
collector:
	go run ./cmd/collector

## analyze: buckets → recommendations, superseding stale pending ones
analyze:
	go run ./cmd/analyze

## costscan: discover unmanaged cloud-cost opportunities (fixture source by default)
costscan:
	go run ./cmd/costscan

## devseed: populate local dev Postgres with fresh fixture data
devseed:
	go run ./cmd/devseed

## api: serve the REST API on CONSIZE_LISTEN_PORT (default 8080)
api:
	go run ./cmd/api
