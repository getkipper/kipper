GO_MODULES := kip console-api controller gateway sidecar kipper-poll datamover

# VERSION stamps the kip and console-api binaries so the CLI's version
# handshake can detect a skew against the cluster. Override with `make
# VERSION=v1.2.3 build`; defaults to the current git description.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test lint

## build: compile every Go module and the web console
build:
	@for m in $(GO_MODULES); do echo "==> build $$m"; (cd $$m && go build -ldflags "$(GO_LDFLAGS)" ./...) || exit 1; done
	@echo "==> build console"; cd console && npm run build

## test: run the Go unit tests for every module and the console unit tests
test:
	@for m in $(GO_MODULES); do echo "==> test $$m"; (cd $$m && go test ./...) || exit 1; done
	@echo "==> test console"; cd console && npm run test

## lint: run golangci-lint per module, eslint and type-check the console
lint:
	@for m in $(GO_MODULES); do echo "==> lint $$m"; (cd $$m && golangci-lint run ./...) || exit 1; done
	@echo "==> lint console"; cd console && npm run lint
	@echo "==> type-check console"; cd console && npm run type-check
