.PHONY: all run build test test-coverage fmt lint audit snapshot docker-build release-dry clean

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

snapshot:
	goreleaser build --snapshot --clean

# Build local base and standalone images from snapshot binaries.
docker-build: snapshot
	BUILDX_BUILDER= docker build \
		--build-context lpgw-amd64=./dist/lpgw_linux_amd64_v1 \
		--build-context lpgw-arm64=./dist/lpgw_linux_arm64_v8.0 \
		--file ./docker/base.Dockerfile \
		--tag lightpanda-gateway:snapshot-base \
		.
	BUILDX_BUILDER= docker build \
		--build-arg BASE_IMAGE=lightpanda-gateway:snapshot-base \
		--file ./docker/standalone.Dockerfile \
		--tag lightpanda-gateway:snapshot-standalone \
		.

release-dry:
	goreleaser release --snapshot --clean

clean:
	rm -f $(BINARY_NAME)
	rm -rf ./dist/
	rm -f coverage.out coverage.html
