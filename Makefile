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

# Tidy and vendor dependencies
vendor:
	go mod tidy
	go mod vendor

# Check formatting (vendored deps are intentionally excluded: vendor/ is not
# gofmt-clean by design, and it is gitignored/regenerated).
fmt:
	@unformatted="$$(git ls-files '*.go' ':!vendor/**' | xargs gofmt -l)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed in:"; echo "$$unformatted"; exit 1; \
	fi; \
	echo "All tracked Go files are gofmt-clean"

# Run all checks
ci: lint test

# Default target
all: build
