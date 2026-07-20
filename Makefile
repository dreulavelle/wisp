SHELL := /bin/bash

PLUGIN      := silo-plugin-wisp
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST        := dist
GOFLAGS     := -trimpath
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: all build plugin dist zip test e2e lint clean

all: test build

## build — plugin binary for the host platform
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(PLUGIN) ./cmd/$(PLUGIN)

## dist — release binaries for the platforms the manifest declares
dist:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(PLUGIN)-linux-amd64 ./cmd/$(PLUGIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(PLUGIN)-linux-arm64 ./cmd/$(PLUGIN)
	@cd $(DIST) && sha256sum $(PLUGIN)-linux-* > checksums.txt
	@echo "built $(VERSION):" && ls -1 $(DIST)

## zip — installable archive for Silo's Admin -> Plugins -> Manual Install.
## Layout is flat: manifest.json plus a binary named `plugin` at the archive
## root, which is what the installer expects.
## Uses python's zipfile rather than the zip(1) binary so packaging needs no
## system dependency beyond what building already requires.
zip: dist
	@python3 -c 'import zipfile,sys,os; \
		arch=os.popen("go env GOARCH").read().strip(); \
		z=zipfile.ZipFile("$(DIST)/$(PLUGIN).zip","w",zipfile.ZIP_DEFLATED); \
		z.write("cmd/$(PLUGIN)/manifest.json","manifest.json"); \
		info=zipfile.ZipInfo("plugin"); info.external_attr=0o755<<16; info.compress_type=zipfile.ZIP_DEFLATED; \
		z.writestr(info, open("$(DIST)/$(PLUGIN)-linux-"+arch,"rb").read()); \
		z.close()'
	@echo "packaged $(DIST)/$(PLUGIN).zip"

test:
	go test ./...

## e2e — full pipeline against a throwaway Silo. Needs Docker.
e2e:
	./test/e2e/run.sh

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

clean:
	rm -rf $(DIST) $(PLUGIN)
