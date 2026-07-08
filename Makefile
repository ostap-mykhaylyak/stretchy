VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: build build-linux test run clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/stretchy ./cmd/stretchy

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/stretchy-linux-amd64 ./cmd/stretchy

test:
	go test -race ./...

run: build
	./bin/stretchy --config config.yaml

clean:
	rm -rf bin dist
