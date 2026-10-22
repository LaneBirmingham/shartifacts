.PHONY: help build run check test race frontend up down logs browser-test
help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-13s %s\n", $$1, $$2}'
build: ## build bin/shartifacts
	go build -trimpath -o bin/shartifacts .
run: ## run from source on :8080 with ./data
	go run .
check: ## vet, test and build
	go vet ./...
	go test ./...
	go build ./...
test: ## Go tests
	go test ./...
race: ## Go tests with the race detector
	go test -race ./...
frontend: ## rebuild web/vendor from the locked npm dependencies
	npm ci
	npm run build
browser-test: ## the browser suite (needs Chrome or Playwright Chromium)
	npm ci
	npm run test:browser
up: ## docker compose up (builds as needed)
	docker compose up -d --build
down: ## docker compose down
	docker compose down
logs: ## follow the shartifacts container's log
	docker compose logs -f shartifacts
