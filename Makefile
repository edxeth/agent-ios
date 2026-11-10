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

# iOS Simulator tests (macOS only) — boots an iOS Simulator and starts iwdp,
# then runs the e2e suite with the simulator tests (the shared helpers of
# internal/tools against real Safari, and the proxy listing tests).
# Setup's exit status is checked before its output is evaluated: a failed
# setup, or one that sets no URL, fails the target instead of letting the
# simulator tests skip.
# The automatic teardown passes the run token that setup printed, so it
# acts only on this run's lease: never on another run's Simulator, also
# when this setup was refused. One Simulator run per user account at a
# time; another run is refused through the lease.
# Usage: make test-e2e
# To debug one test by hand: run make sim-setup; only when it succeeds,
# export the variables that it prints and run go test with
# -tags=simulator and -run for that test; then tear down with the setup
# script's --teardown-run mode and the printed IWDP_SIM_RUN_TOKEN.
# make sim-teardown is the manual recovery after a crashed run only.
sim-setup:
	@unset IWDP_SIM_WS_URL IWDP_SIM_PAGE_URL IWDP_SIM_RUN_TOKEN; \
		out=$$(./scripts/sim-setup.sh) || { rc=$$?; echo "sim-setup.sh failed (exit $$rc); see its messages above" >&2; exit $$rc; }; \
		eval "$$out"; \
		if [ -z "$$IWDP_SIM_WS_URL" ] || [ -z "$$IWDP_SIM_PAGE_URL" ]; then \
			echo "sim-setup.sh set no IWDP_SIM_WS_URL or IWDP_SIM_PAGE_URL" >&2; ./scripts/sim-setup.sh --teardown-run "$$IWDP_SIM_RUN_TOKEN"; exit 1; \
		fi; \
		echo "$$out"

# Manual recovery after a crashed run: tears down whatever run holds the
# lease. Never run it while another run may still be active.
sim-teardown:
	@./scripts/sim-setup.sh --teardown

test-e2e:
	@echo "==> Setting up iOS Simulator..."
	@unset IWDP_SIM_WS_URL IWDP_SIM_PAGE_URL IWDP_SIM_RUN_TOKEN; \
		out=$$(./scripts/sim-setup.sh) || { rc=$$?; echo "sim-setup.sh failed (exit $$rc); see its messages above" >&2; exit $$rc; }; \
		eval "$$out"; \
		if [ -z "$$IWDP_SIM_WS_URL" ] || [ -z "$$IWDP_SIM_PAGE_URL" ]; then \
			echo "sim-setup.sh set no IWDP_SIM_WS_URL or IWDP_SIM_PAGE_URL" >&2; ./scripts/sim-setup.sh --teardown-run "$$IWDP_SIM_RUN_TOKEN"; exit 1; \
		fi; \
		echo "==> Running e2e tests..."; \
		go test -tags=simulator ./e2e/ -v -count=1 -timeout=300s; rc=$$?; \
		echo "==> Tearing down..."; \
		./scripts/sim-setup.sh --teardown-run "$$IWDP_SIM_RUN_TOKEN"; \
		exit $$rc

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
