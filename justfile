set positional-arguments

default:
    just --list

build:
    mkdir -p bin
    go build -trimpath -o bin/cake-compact ./cmd/cake-compact

install:
    go install -trimpath ./cmd/cake-compact

run *args:
    go run ./cmd/cake-compact "$@"

test:
    go test ./...

test-race:
    go test -race ./...

# Replay-validate generated checkpoints and exercise in-place appends with a
# real Cake binary. Defaults to the sibling Cake worktree build; override with
# CAKE_BIN. Pass an absolute default: a Go test runs in its package directory,
# so a relative path would resolve against internal/compact.
test-cake:
    CAKE_BIN="${CAKE_BIN:-$PWD/../cake/target/debug/cake}" go test -count=1 -run RealCake ./...

fmt:
    gofmt -w .

fmt-check:
    test -z "$(gofmt -l .)"

vet:
    go vet ./...

ci: fmt-check vet test build
