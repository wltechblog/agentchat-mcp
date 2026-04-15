.PHONY: build build-server build-bridge run test clean docker docker-up docker-down

build: build-server build-bridge

build-server:
	go build -o bin/agentchat-server ./cmd/server

build-bridge:
	go build -o bin/agentchat-mcp-bridge ./cmd/agentchat-mcp-bridge

run: build-server
	./bin/agentchat-server

test:
	go test -race ./...

clean:
	rm -rf bin/

docker:
	docker build -t agentchat-mcp .

docker-up:
	docker compose up -d

docker-down:
	docker compose down
