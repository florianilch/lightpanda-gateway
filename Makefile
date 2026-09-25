.PHONY: all run build test test-coverage fmt lint audit clean

BINARY_NAME := lpgw
MAIN := ./cmd/lpgw

all: test build

run:
	go run $(MAIN)

build:
	go build -trimpath -ldflags "-s -w" -o ./dist/$(BINARY_NAME) $(MAIN)

test:
	go test -race ./...

test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

fmt:
	golangci-lint fmt ./...

lint:
	golangci-lint run

audit:
	go mod tidy -diff
	go mod verify
	go tool govulncheck ./...

clean:
	rm -f $(BINARY_NAME)
	rm -rf ./dist/
	rm -f coverage.out coverage.html
