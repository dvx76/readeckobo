# Build the application
build:
	go build -o readeckobo ./cmd/readeckobo

# Run tests
test:
	go test ./...

test-coverage:
	./scripts/check-coverage.sh 60

test-coverage-full:
	./scripts/check-coverage.sh --mode=functions 60

test-all:
	go test -count=1 ./...

# Run linter
lint:
	golangci-lint run

# Build the on-device agent installer (cross-compiled linux/arm + KoboRoot.tgz).
agent:
	./scripts/build-agent.sh

# Alias for `make agent`
dist: agent

# Re-download the Mozilla CA bundle embedded in the agent
# (cmd/readeckobo-agent/cabundle.pem, registered via x509.SetFallbackRoots —
# see cmd/readeckobo-agent/certs.go). Requires curl; fails loudly on error.
# Deliberately NOT wired into `agent`: builds stay hermetic with the
# committed bundle. Commit the updated file afterwards.
refresh-cabundle:
	curl -fsSL https://curl.se/ca/cacert.pem -o cmd/readeckobo-agent/cabundle.pem

# Check formatting (vendored deps are intentionally excluded: vendor/ is not
# gofmt-clean by design, and it is gitignored/regenerated).
fmt:
	@unformatted="$$(git ls-files '*.go' ':!vendor/**' | xargs gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed in:"; echo "$$unformatted"; exit 1; \
	fi; \
	echo "All tracked Go files are gofmt-clean"

# Tidy and vendor dependencies
vendor:
	go mod tidy
	go mod vendor

# Run all checks
ci: lint test

# Default target
all: build
