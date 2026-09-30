VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/roganis/ez2pyro/internal/agent.Version=$(VERSION)
export CGO_ENABLED := 0

.PHONY: all build test test-short vet dist clean

all: vet test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/linkdoctor ./cmd/linkdoctor

dist:
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/linkdoctor-linux-amd64 ./cmd/linkdoctor
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/linkdoctor-windows-amd64.exe ./cmd/linkdoctor

test:
	go test ./...

# Skips the loopback traffic tests (useful on slow or shared machines).
test-short:
	go test -short ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

clean:
	rm -rf bin dist
