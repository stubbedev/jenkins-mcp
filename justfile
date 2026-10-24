_default:
    @just --list

# Build the binary.
build:
    go build -o jenkins-mcp .

# Run tests.
test:
    npm test

# Format. gofmt is the whole formatter here: no golangci config in this repo, so
# `just check` gates on gofmt + vet, exactly like ci.yml does.
fmt:
    gofmt -w .

# End-to-end: drive the built binary over stdio and assert tools/list.
smoke:
    npm run smoke

# Everything CI runs, read-only on formatting so it fails instead of fixing.
check:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(gofmt -l .)
    if [ -n "$out" ]; then
        echo "code is not formatted; run 'just fmt':"
        printf '%s\n' "$out"
        exit 1
    fi
    go vet ./...
    go test -race ./...
    go build -o jenkins-mcp .
    node scripts/smoke.mjs
    node --test 'scripts/*.test.mjs'

nix-build:
    nix build .#default -L

nix-check:
    nix flake check --print-build-logs

clean:
    rm -f jenkins-mcp

# ─────────────────────────── Release ───────────────────────────
# package.json holds the version: nix/package.nix reads it, and publish.yml refuses a
# tag that disagrees with it. `npm version` bumps, commits and tags in one go
# (with `preversion` running vet + test + smoke first), so these are wrappers
# over the npm scripts rather than a second implementation of the same flow.

release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CUR=$(node -p "require('./package.json').version")
    IFS=. read -r MAJOR MINOR PATCH <<<"$CUR"
    echo "Current version: v$CUR"
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

release-patch:
    npm run release:patch

release-minor:
    npm run release:minor

release-major:
    npm run release:major
