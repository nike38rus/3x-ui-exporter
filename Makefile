export GOPATH := $(CURDIR)/.cache/gopath
export GOMODCACHE := $(CURDIR)/.cache/mod
export GOCACHE := $(CURDIR)/.cache/build
export GOTMPDIR := $(CURDIR)/.cache/tmp

.PHONY: test build linux prepare
prepare:
	mkdir -p .cache/tmp bin
test: prepare
	go test -race ./...
	go vet ./...
build: prepare
	go build -trimpath -o bin/3x-ui-exporter .
linux: prepare
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/3x-ui-exporter-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/3x-ui-exporter-linux-arm64 .
