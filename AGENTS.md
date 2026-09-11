# AGENTS — dnsproxy

## Quick commands

| Action | Command |
|---|---|
| Build binary | `make build` |
| Run tests | `make test` (or `RACE=1 make test` for race) |
| Full check (lint + test) | `make go-check` |
| Run linter only | `make go-lint` |
| Download deps | `make go-deps` |
| Init git hooks | `make init` |
| Docker: pull image | `docker pull adguard/dnsproxy` |
| Docker: run default | `docker run -p 53:53/tcp -p 53:53/udp adguard/dnsproxy` |
| Docker: run with args | `docker run -p 53:53/tcp -p 53:53/udp adguard/dnsproxy -u 8.8.8.8:53` |

## Test notes

- Tests use `--count=2 --shuffle=on --coverprofile=./cover.out` (`scripts/make/go-test.sh:40-45`)
- Integration tests: set `TEST_INTEGRATION=1` (adds `--coverpkg=./...`)
- Race tests: `RACE=1` flag (e.g. `RACE=1 make test`)
- Test timeout default: `---timeout=2m` (`go-test.sh:44`)

## Lint pipeline order matters

`make go-check` runs `go-lint` then `go-test` (`Makefile:77`).  
`make go-lint` runs in this order (see `scripts/make/go-lint.sh`):
1. `blocklist_imports`, `method_const`, `underscores`
2. `gofumpt`
3. `go vet`
4. `govulncheck` (non-reproducible; set `IGNORE_NON_REPRODUCIBLE=1` to skip)
5. `gocyclo --over 10`
6. `gocognit --over 10`
7. `ineffassign`
8. `unparam`
9. `misspell` on Makefile/*.md/*.yaml/etc.
10. `nilness`
11. `fieldalignment`
12. `gosec --exclude-generated --fmt=golint --quiet`
13. `errcheck`
14. `staticcheck` cross-OS matrix (darwin/freebsd/linux/openbsd/windows)

## Architecture pointers

- Main entry: `main.go` → `internal/cmd.Main()` (`internal/cmd/cmd.go`)
- Key packages: `proxy/`, `upstream/`, `internal/`
- LD flags set from `github.com/AdguardTeam/golibs/version` (`scripts/make/go-build.sh:59-65`)
- Build: `GOFLAGS -p=${PARALLELISM}` for parallelism (`go-build.sh:73-76`)
- Binary output: `OUT=dnsproxy` (`Makefile:27`)

## Env / tooling

- `GOTOOLCHAIN=go1.27.1` (`Makefile:28`)
- `GOAMD64=v1` (`Makefile:24`)
- `GOPROXY=https://proxy.golang.org|direct` (`Makefile:25`)
- `GOTELEMETRY=off` (`Makefile:26`)
- CI runs `go-os-check` on linux, darwin, freebsd, openbsd, windows (`Makefile:81-87`)
- `.gitignore` ignores: `./bin/ ./test-reports/ ./tmp/ *.out *.exe *.test build dnsproxy` (`go.mod:99-103`)

## Git hooks (pre-commit / pre-merge-commit)

- `scripts/hooks/pre-commit`: warns about TODO/FIXME, checks unstaged changes, runs lint_staged_changes
- `scripts/hooks/pre-merge-commit`: runs `check_unstaged_changes` + `lint_staged_changes`
- On `*.go` `*.mod` `Makefile` staged changes: runs `make go-os-check go-lint go-test` (`scripts/hooks/helper.sh:103-105`)
- On `*.md` staged changes: runs `make md-lint` (`scripts/hooks/helper.sh:76`)
- On `*.sh` staged changes: runs `make sh-lint` (`scripts/hooks/helper.sh:80`)
- On `*.json` `*.md` `*.yaml` `.dockerignore` `.gitignore` `Makefile` staged changes: runs `make txt-lint` (`scripts/hooks/helper.sh:100`)

## Common gotchas

- `--upstream-mode` values: `load_balance` (default), `parallel`, `fastest_addr`
- `--bogus-nxdomain` transforms upstream IPs matching the given CIDRs/subnets into NXDOMAIN
- `--pending-requests-enabled` disables duplicate-query tracking — introduces cache‑poisoning vulnerability if disabled
- `--dns64` requires `--use-private-rdns` + `--private-rdns-upstream` for reverse DNS of private addresses
- Cache: default size 64k (`--cache-size=64`), optimistic cache default TTL 30s (`--optimistic-answer-ttl`)
- Always run `make go-deps` or `go mod download` before building/testing for the first time

## Sharded cache (proxy/cache.go)

- 64 shards, each with own `itemsLock` and `itemsWithSubnetLock`
- Key hashed via FNV-1a → `hashKey(key) % numShards`
- Shard size = `conf.size / numShards`
- `get`/`set`/`getWithSubnet`/`setWithSubnet` all delegate to correct shard
- `clearItems`/`clearItemsWithSubnet` iterate all shards
- ECS subnet cache also sharded

## Test flakiness

- `TestDNSOverQUIC_serverRestart` (upstream/doq_internal_test.go:158): timeout was 100ms, changed to 500ms to avoid flakiness on slow CI runners
- Re-run with `-count=2` if flaky