export GOPATH := $(CURDIR)/.cache/gopath
export GOMODCACHE := $(CURDIR)/.cache/mod
export GOCACHE := $(CURDIR)/.cache/build
export GOTMPDIR := $(CURDIR)/.cache/tmp

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build linux prepare licenses release
prepare:
	mkdir -p .cache/tmp bin
test: prepare
	go test -race ./...
	go vet ./...
build: prepare
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/3x-ui-exporter .
linux: prepare
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/3x-ui-exporter-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/3x-ui-exporter-linux-arm64 .
licenses: prepare
	python3 scripts/licenses.py
release: prepare
	bash scripts/release.sh $(VERSION)
