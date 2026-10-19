VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X github.com/iryzhkov/jocasta/internal/buildinfo.Version=$(VERSION) -X github.com/iryzhkov/jocasta/internal/buildinfo.Commit=$(COMMIT)
.PHONY: build test check cross release
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/jocasta ./cmd/jocasta
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/jocastad ./cmd/jocastad
test:
	go test -race ./...
check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test ./...
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/jocasta_$(VERSION)_linux_amd64 ./cmd/jocasta
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/jocasta_$(VERSION)_linux_arm64 ./cmd/jocasta
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/jocastad_$(VERSION)_linux_amd64 ./cmd/jocastad
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/jocastad_$(VERSION)_linux_arm64 ./cmd/jocastad
	cd dist && sha256sum jocasta* > checksums.txt
