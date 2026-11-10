.PHONY: build test test-e2e test-coverage coverage-all test-cdpcompat \
       sim-setup sim-teardown lint fmt tidy install clean release release-snapshot docs

# Build
build:
	go build -o bin/iwdp-cli ./cmd/iwdp-cli

# Run
run-cli:
	go run ./cmd/iwdp-cli $(ARGS)

# Install iwdp-cli
install:
	go install ./cmd/iwdp-cli

# Test
test:
	go test ./... -v -count=1

test-coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

# One coverage profile (coverage.out) from the unit tests and the WebKitGTK
# suites, including the binaries they start; needs the e2e/webkitgtk tools.
coverage-all:
	./scripts/coverage-all.sh coverage.out

# CDP compatibility suite (design 8.4, gate IWDP_CDP_COMPAT=1): the pinned
# CDP clients against the CDP endpoint; see e2e/cdpcompat/README.md. Needs
# Node.js 24+ and npm; the lockfile pins every client, agent-browser
# included. npm ci runs when the lockfile changed, without install scripts
# (agent-browser's postinstall can download a binary and patch global bin
# links); the package ships its binaries, which get their execute bit here.
CDPCOMPAT := e2e/cdpcompat
test-cdpcompat: $(CDPCOMPAT)/node_modules/.package-lock.json
	IWDP_CDP_COMPAT=1 go test -tags=cdpcompat ./$(CDPCOMPAT)/ -v -count=1 -timeout=900s

$(CDPCOMPAT)/node_modules/.package-lock.json: $(CDPCOMPAT)/package.json $(CDPCOMPAT)/package-lock.json
	@command -v node >/dev/null && command -v npm >/dev/null || \
		{ echo "test-cdpcompat needs Node.js 24 or later with npm on PATH (see $(CDPCOMPAT)/README.md)" >&2; exit 1; }
	cd $(CDPCOMPAT) && PUPPETEER_SKIP_DOWNLOAD=1 npm ci --ignore-scripts --no-audit --no-fund
	chmod +x $(CDPCOMPAT)/node_modules/agent-browser/bin/agent-browser-*

# E2E tests — boots iOS Simulator + iwdp, runs all tools against real Safari.
# Usage: make test-e2e
#   or:  make sim-setup && go test -tags=simulator ./e2e/ -v && make sim-teardown
sim-setup:
	@eval "$$(./scripts/sim-setup.sh)" && echo "IWDP_SIM_WS_URL=$$IWDP_SIM_WS_URL"

sim-teardown:
	@./scripts/sim-setup.sh --teardown

test-e2e:
	@echo "==> Setting up iOS Simulator..."
	@eval "$$(./scripts/sim-setup.sh)" && \
		echo "==> Running e2e tests..." && \
		go test -tags=simulator ./e2e/ -v -count=1 -timeout=300s && \
		echo "==> Tearing down..." && \
		./scripts/sim-setup.sh --teardown
	@./scripts/sim-setup.sh --teardown 2>/dev/null || true

# Command reference (docs/commands.md) from the command registry
docs:
	go run ./internal/commands/refgen docs/commands.md

# Lint (golangci-lint v2.15 or later, built with Go 1.27)
lint:
	golangci-lint run ./...

# Format
fmt:
	gofumpt -w .

# Tidy
tidy:
	go mod tidy

# Release (dry run)
release-snapshot:
	goreleaser release --snapshot --clean

# Release
release:
	goreleaser release --clean

# Clean
clean:
	rm -rf bin/ coverage.out coverage.html dist/
