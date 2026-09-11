.PHONY: build build-server build-bridge build-cli run test lint check clean docker docker-up docker-down

build: build-server build-bridge build-cli

build-server:
	go build -o bin/agentchat-server ./cmd/server

build-bridge:
	go build -o bin/agentchat-mcp-bridge ./cmd/agentchat-mcp-bridge

build-cli:
	go build -o bin/agentchat-cli ./cmd/agentchat-cli

run: build-server
	./bin/agentchat-server

test:
	go test -race ./...

# lint fails on any unformatted file or vet finding.
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...

# check is what CI runs.
check: lint test

clean:
	rm -rf bin/

docker:
	docker build -t agentchat-mcp .

docker-up:
	docker compose up -d

docker-down:
	docker compose down
