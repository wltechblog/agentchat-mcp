FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
ARG COMMIT_HASH=unknown
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-X main.commitHash=$COMMIT_HASH" -o /agentchat-server ./cmd/server
RUN CGO_ENABLED=0 go build -o /agentchat-cli ./cmd/agentchat-cli

FROM alpine:3.20
RUN apk --no-cache add ca-certificates
COPY --from=builder /agentchat-server /usr/local/bin/agentchat-server
COPY --from=builder /agentchat-cli /usr/local/bin/agentchat-cli
EXPOSE 8080
ENTRYPOINT ["agentchat-server"]
