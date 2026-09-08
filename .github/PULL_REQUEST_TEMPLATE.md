## What this changes and why

<!-- One or two sentences. Link the issue this addresses, if any. -->

## Which package(s)

<!-- e.g. core/snapshot, internal/cli. See CONTRIBUTING.md's ownership table
     if you are unsure which area this falls under. -->

## How you tested this

<!-- go test ./...? hack/linux-test.sh? DEBARK_E2E=1? Which OS? -->

- [ ] `go build ./...` and `go vet ./...` pass locally
- [ ] `go test ./...` passes locally
- [ ] Ran with `DEBARK_E2E=1` (via `hack/linux-test.sh`), if this touches apt/dpkg/gpg/container code — or N/A
- [ ] `gofmt -l .` reports nothing changed by this PR
- [ ] `golangci-lint run` is clean for the packages touched (or `make lint`)

## Checklist

- [ ] Every commit is signed off (`git commit -s`) — see [DCO](../DCO) and [CONTRIBUTING.md](../CONTRIBUTING.md)
- [ ] This does not add a new Go dependency, **or** the dependency was discussed first (not a drive-by `go get`)
- [ ] This does not print to stdout/stderr from `core/`, and `core/` still does not import `internal/cli`
- [ ] Errors returned from a code path a command can fail on are classified with `core/dferr`
- [ ] Anything hashed or signed goes through `core/canonical` (never raw `json.Marshal`/`json.MarshalIndent`)
- [ ] If this changes a schema under `api/`, a frozen file listed in `docs/dev/contract-brief.md`, or an existing exported signature in an `api.go`: an ADR is included under `docs/adr/` explaining why
- [ ] If this changes deterministic output (manifest, repository metadata, lock), a test proves two runs still produce byte-identical results
- [ ] No telemetry, analytics, or update-check code — anywhere, even behind a flag

## Anything the reviewer should look at closely

<!-- Optional: tricky edge cases, things you are unsure about, alternatives you considered. -->
