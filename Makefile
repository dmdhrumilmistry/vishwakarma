VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE ?= ghcr.io/dmdhrumilmistry/vishwakarma
XTERM_VERSION := 6.0.0
XTERM_FIT_VERSION := 0.11.0

.PHONY: build test lint js image run vendor clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vishwakarma ./cmd/vishwakarma

test:
	go vet ./...
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"

# Syntax check the console modules (no build step, so nothing else runs them).
js:
	for f in internal/web/static/js/*.js; do node --check "$$f" || exit 1; done

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Run against the cluster in your current kubeconfig. Credentials come from
# dev/env (git-ignored); see docs/development.md.
run: build
	set -a && . ./dev/env && set +a && ./bin/vishwakarma serve

# Refresh the vendored xterm.js files.
vendor:
	tmp=$$(mktemp -d) && cd $$tmp && \
	npm pack @xterm/xterm@$(XTERM_VERSION) @xterm/addon-fit@$(XTERM_FIT_VERSION) >/dev/null && \
	for f in *.tgz; do mkdir -p $${f%.tgz} && tar xzf $$f -C $${f%.tgz} --strip-components=1; done && \
	cd - >/dev/null && \
	cp $$tmp/xterm-xterm-$(XTERM_VERSION)/lib/xterm.mjs $$tmp/xterm-xterm-$(XTERM_VERSION)/css/xterm.css \
	   $$tmp/xterm-addon-fit-$(XTERM_FIT_VERSION)/lib/addon-fit.mjs internal/web/static/vendor/xterm/ && \
	sed -i '/sourceMappingURL=/d' internal/web/static/vendor/xterm/*.mjs && rm -rf $$tmp

clean:
	rm -rf bin dist
