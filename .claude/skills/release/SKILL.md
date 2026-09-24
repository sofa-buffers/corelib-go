---
name: release
description: Cut a release of corelib-go — preflight gates, the annotated tag, the GitHub release notes, and the post-tag proxy warm-up. Use when the user asks to release, tag, or publish a new version of this library, or asks what a release needs.
---

# Releasing corelib-go

## The one thing to know first

**No file in this tree carries the release version.** For a Go module the version
*is* the tag, and CORELIB_PLAN §12.3 makes that normative: Go repos MUST NOT carry
`version-consistency.yml`, because "wherever the tag is the single source of truth
for the version, there is no second copy to disagree with it."

So there is no version to bump before tagging. Do not invent one — do not add a
`Version` constant, a `VERSION` file, or a version line to the README. `APIVersion`
in `types.go` is the **wire-format** version (`1`, CORELIB_PLAN §6.2) and is
unrelated to the release version; it changes only when the spec changes it.

The two released tags so far (`v0.9.0`, `v0.10.0`) sit directly on merge commits
that were already `main`'s head. There were no release-prep commits, and there
should not be.

## Step 0 — settle the version number

Ask the user for it if they did not say. Two rules constrain it:

- **The family moves together.** The version is coordinated across all
  `corelib-*` ports and `sofabgen`; `v0.10.0`'s notes read "Aligns this library
  with the rest of the SofaBuffers family at **0.10.0**". If the user names a
  version that does not match what the rest of the family is on, say so and ask
  before tagging.
- **Pre-1.0, a minor bump may break API or wire output.** That is the stated rule
  in the existing release notes. A breaking change therefore goes in a minor bump
  (`0.10.0` → `0.11.0`), not a major one, while the library is below 1.0.

## Step 1 — preflight gates

Run all of these. Do not tag until every one passes; report any failure with its
output rather than working around it.

```bash
# On main, clean, and level with the remote.
git rev-parse --abbrev-ref HEAD          # must be main
git status --porcelain                   # must be empty
git pull -p
git rev-list --left-right --count main...origin/main   # must be 0	0
```

```bash
# The full CI matrix, locally. CI covers these, but a tag pushes no CI run of
# its own (every workflow here triggers on push-to-main / PR, never on tags),
# so main's own green run is the only signal — confirm it, and re-run locally.
gofmt -l .                               # must print nothing
go vet ./...
go build ./...
go test -race -count=1 ./...
go test -race -count=1 ./tests/          # vector suite, for its reported counts

# The documented footprint build (CORELIB_PLAN §6.4) is a real CI leg.
go vet  -tags sofab_no_strict_utf8 ./...
go build -tags sofab_no_strict_utf8 ./...
go test -race -count=1 -tags sofab_no_strict_utf8 ./...
```

The matrix's other leg is Go 1.21, the floor `go.mod` declares. There is normally
no 1.21 toolchain here, so that leg's signal comes from main's CI run — which is
the next check, not something to skip.

```bash
# main's CI must actually be green — a tag runs nothing itself.
gh run list --branch main --limit 5
```

**The shared vectors must be current.** `assets/test_vectors.json` is a verbatim
copy of the file `corelib-c-cpp` owns, and `shared-vectors.yml` only checks it on
PRs that touch it plus once a day — so a tag can otherwise ship a stale corpus:

```bash
curl -fsSL -o /tmp/upstream-vectors.json \
  https://raw.githubusercontent.com/sofa-buffers/corelib-c-cpp/main/assets/test_vectors.json
sha256sum assets/test_vectors.json /tmp/upstream-vectors.json   # the two must match
```

If they differ, stop. Refreshing the copy is a normal PR into main
(`chore(vectors): sync ...`), not something to fold into a release.

## Step 2 — the version-carrying-files audit

Normally this finds nothing. Check anyway, because these are the only places a
version could legitimately need to move:

| File | When it changes | Also update |
|---|---|---|
| `go.mod` `go 1.21` | only when the **minimum Go** rises | README `### Requirements` ("Go **1.21+**") and the `ci.yml` matrix floor |
| `go.mod` module path | only at **v2.0.0 and above** | see the appendix — this is a big change |
| nothing else | — | — |

```bash
grep -rn 'go 1\.' go.mod
grep -n 'Go \*\*1\.' README.md
grep -n "go: \['" .github/workflows/ci.yml
```

## Step 3 — the README must still be true

CORELIB_PLAN §9 binds the README: every version number, dependency, feature flag
and API name it states MUST match the code. A release is the moment that gets
shipped, so verify the claims the release touches — the exported API surface, the
build tags in the flags table, `MIN_OUTPUT_BUFFER` (§9.6), and the §9.6 memory
contract if anything in `encoder.go` / `istream.go` / `collectors.go` moved.

The coverage badge and the godoc site are automatic (`ci.yml` publishes to the
`badges` branch, `docs.yml` to Pages, both on push to main). Nothing to do.

## Step 4 — tag

Use an **annotated** tag. `v0.9.0` is annotated and `v0.10.0` is lightweight;
annotated is the one to keep, because the message carries the spec citation.

```bash
git tag -a v0.11.0 -m "SofaBuffers corelib 0.11.0

<one paragraph: what changed, citing the governing spec section and the upstream
issue — e.g. 'MESSAGE_SPEC §2/§3/§5.1 (sofa-buffers/documentation#29): ...'>"
git push origin v0.11.0
```

Tag the commit that is `main`'s head and nothing else.

## Step 5 — the GitHub release

```bash
gh release create v0.11.0 --title v0.11.0 --notes-file <path>
```

Follow the shape `v0.10.0` set:

1. One line placing the version in the family, and the reminder that the git tag
   is the source of truth for the version.
2. A **`Breaking since vX.Y.Z`** section when there is one, under the stated
   pre-1.0 rule that a minor bump may break API or wire output.
3. One bullet per change, each naming its **Crucible finding ID** and the
   governing spec section (`Crucible F-0042 / CORELIB_PLAN §4.8`), saying what
   changed in the API or on the wire and what a consumer must do.
4. A closing line on `sofabgen` / the rest of the family if they moved in lockstep.

Draft the notes from `git log <previous-tag>..HEAD` and show them to the user
before creating the release.

## Step 6 — after the tag

```bash
# Publish to the module proxy so pkg.go.dev picks the version up.
GOPROXY=https://proxy.golang.org GOFLAGS=-mod=mod \
  go list -m github.com/sofa-buffers/corelib-go@v0.11.0

# Confirm it resolves for a consumer.
go list -m -versions github.com/sofa-buffers/corelib-go
```

Then tell the user the release URL and that `https://pkg.go.dev/github.com/sofa-buffers/corelib-go@v0.11.0`
may take a few minutes to appear.

## Appendix — v2.0.0 and above

Go's major-version rule: from `v2` on, the major version becomes part of the
module path. That release is not just a tag — it is
`module github.com/sofa-buffers/corelib-go/v2` in `go.mod`, every internal
import path, the README `go get` line and import example, and every downstream
consumer. Flag it to the user as its own PR before any tag is cut.
