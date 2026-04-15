.PHONY: build run test clean docker

build:
	go build -o bin/agentchat-server ./cmd/server

run: build
	./bin/agentchat-server

test:
	go test ./...

clean:
	rm -rf bin/

docker:
	docker build -t agentchat-mcp .

docker-up:
	docker compose up -d

docker-down:
	docker compose down
