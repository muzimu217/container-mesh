# container-mesh — 常用任务
GO ?= go
BIN ?= cmesh
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test vet fmt check e2e e2e-large docker clean

all: check test build

build:
	$(GO) build -trimpath -ldflags "-s -w -X github.com/muzimu217/container-mesh/internal/cli.Version=$(VERSION)" -o $(BIN) ./cmd/cm

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "未格式化:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...

e2e:
	bash tests/run-tests.sh --verbose

e2e-large:
	bash tests/run-tests.sh --quick --large --verbose

docker:
	docker build -t cmesh:latest .

clean:
	rm -f $(BIN)
	rm -rf dist build
