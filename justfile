set shell := ["bash", "-uc"]

# Default recipe - show available commands
default:
    @just --list

# Build all packages
build:
    go build ./...

# Build the absdb CLI into bin/
build-cli:
    go build -o bin/absdb ./cmd/absdb

# Run go vet
vet:
    go vet ./...

# Format all code using treefmt (rewrites files in place)
fmt:
    treefmt --allow-missing-formatter

# Check formatting without modifying the working tree
check-formatted:
    #!/usr/bin/env bash
    # treefmt has no read-only mode: every formatter in treefmt.toml runs in
    # write mode, so `treefmt --fail-on-change` would reformat the very files it
    # claims to be checking. Instead, copy every git-visible file (tracked plus
    # untracked-but-not-ignored, so testdata/ is skipped) into a
    # scratch directory and format the copy. Only the exit status is used; the
    # working tree is never written to. Needs GNU coreutils for `cp --parents`.
    set -euo pipefail
    work="$(mktemp -d)"
    trap 'rm -rf "$work"' EXIT
    git ls-files -c -o --exclude-standard -z | xargs -0 cp --parents -t "$work"
    treefmt -C "$work" --no-cache --fail-on-change --allow-missing-formatter

# Run linters
lint:
    GOCACHE="${GOCACHE:-/tmp/gocache}" GOMODCACHE="${GOMODCACHE:-/tmp/gomodcache}" GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-/tmp/golangci-lint-cache}" golangci-lint run --timeout=5m ./...

# Run linters with auto-fix
lint-fix:
    GOCACHE="${GOCACHE:-/tmp/gocache}" GOMODCACHE="${GOMODCACHE:-/tmp/gomodcache}" GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-/tmp/golangci-lint-cache}" golangci-lint run --fix --timeout=5m ./...

# Ensure go.mod/go.sum are tidy (read-only: -diff does not write)
check-tidy:
    go mod tidy -diff

# Run all tests
test:
    go test ./...

# Run all tests, verbose
test-verbose:
    go test -v ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run tests with coverage
test-coverage:
    go test -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out -o coverage.html

# Fuzz every discovered target for a short budget (default 30s each)
fuzz fuzztime="30s":
    #!/usr/bin/env bash
    # `go test -fuzz` accepts one target in one package per run, so iterate.
    set -euo pipefail
    found=0
    for pkg in $(go list ./...); do
      targets="$(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true)"
      for t in $targets; do
        found=1
        echo "==> fuzzing $t in $pkg for {{ fuzztime }}"
        go test -run '^$' -fuzz "^${t}$" -fuzztime={{ fuzztime }} "$pkg"
      done
    done
    if [ "$found" -eq 0 ]; then echo "no fuzz targets found"; fi

# Run all checks: build, vet, formatting, linting, tests, tidiness
# (every step is read-only, so this is safe locally and on a CI runner)
ci: build vet check-formatted lint test-race check-tidy

# Clean build artifacts
clean:
    rm -f coverage.out coverage.html
    rm -rf bin

# Auto-fix what can be auto-fixed (rewrites files)
fix:
    just lint-fix
    just fmt
