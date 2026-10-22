# Release preparation

This is the whole procedure. If you are an agent asked to "cut a release",
follow it literally — every step here is enforced by something in the repo, and
the enforcement is listed so you can tell a real failure from a stale doc.

## Branches

- **`dev` is the development line.** One line, rebased, no merge commits: work
  lands with `git rebase` onto `dev`, never with a merge that drags the other
  branch's history in.
- **`main` only receives releases.** It moves when a release PR merges. Do not
  develop on `main` and do not fast-forward it from `dev`.
- **The release commit must come back to `dev`.** `make prepare-release` runs on
  the release branch, and that branch merges to `main`; `dev` then needs the
  same commit (rebase or cherry-pick it). Skipping this is why `dev` still
  carried v0.2.7's notes under `## Unreleased` after v0.2.7 shipped, and would
  have re-shipped them in the next release. Bouncing the commit back is what
  empties `Unreleased` and brings `server.json` current on the dev line.
  `TestUnreleasedDoesNotRepeatAPublishedEntry` fails when this is skipped, which
  it has been twice (v0.2.7, v0.2.8-rc.3) — the paragraph above did not prevent
  either, so the check is now mechanical. If the test is red, the release commit
  is on `main` and not on `dev`: cherry-pick it rather than deleting the
  duplicate entries by hand.

## Ordinary development

User-facing changes accumulate under `## Unreleased` in `CHANGELOG.md`, and the
Unreleased section must be written **before** the release is cut — `make
prepare-release` only moves those notes into a dated section, it never writes
them.

**What goes in: the change since the last release.** `git log vX.Y.Z..HEAD`
(plus `git diff --stat` for scope) is the input. That is the standard for both
whether a change is listed and how it is described: if a reader of the previous
release would not notice the difference, it does not belong here. Dependency
bumps, refactors with no visible effect, test-only changes and CI tweaks are
noise at this granularity.

**How detailed: one short paragraph per entry, two or three sentences.** Enough
for someone to know what changed and whether it affects them, not a design
note. This file is read by users scanning a release; the reasoning belongs in
the commit message and the code comment, which is where the next person will
look when they touch that code. If a note has grown past a paragraph, the
detail is in the wrong place.

Aim for a handful of entries per release, grouped under the existing headings
(`### 新增` / `### 改进` / `### 修复`). The long entries already in this file's
history are the previous style; do not add to them.

**Compressing a big cycle.** Some releases carry too many changes to shrink to
a paragraph each — a feature built over several commits, plus the fixes to
the fixes. Write those entries from the **difference between the two releases**,
not from the commit list: collapse the back-and-forth and describe the end
state. Work that was introduced and corrected inside the same cycle is not two
entries — a reader coming from the previous release never saw the broken
intermediate, so `v0.2.7 -> v0.2.8` is the only comparison that exists. A
subsystem rebuilt three times gets one entry saying what it does now, not three
saying what happened. The same rule removes a commit's worth of noise when the
end state is unchanged (a refactor with no visible effect, work reverted before
release).

`server.json` keeps naming the latest **final** release while development
continues.

## Cutting a release

Open a release PR **from a branch named `release/vX.Y.Z`**, or title it
`release: vX.Y.Z`. For a pre-release the name is the pre-release itself
(`release/v0.2.8-rc.1`). The name/title is the signal CI uses to require that
exact version; without a release signal CI cannot tell an ordinary PR from a
release PR whose metadata was forgotten.

1. `make prepare-release RELEASE_VERSION=vX.Y.Z` — moves the current Unreleased
   notes into a dated `## vX.Y.Z — YYYY-MM-DD` section and updates the version
   and OCI identifier in `server.json`.
2. Review and edit the notes. They are the release's permanent record — check
   them against `git log <last release>..HEAD`: every user-visible change
   present, nothing that only matters to a developer.
3. `make check-release RELEASE_VERSION=vX.Y.Z`, then
   `python3 -m unittest discover -s scripts -p '*_test.py'`.
4. Merge only after the `Test` workflow passes. In GitHub branch protection,
   require `test (ubuntu-latest)`, `test (macos-latest)` and
   `test (windows-latest)` on both `main` and `dev`. A workflow that merely runs
   does not prevent merging.
5. Tag the merged commit `vX.Y.Z`, and bring the release commit back to `dev`
   (see Branches above). Committing and pushing still require the human approval
   described in `AGENTS.md`.

Both tag-triggered workflows check the committed files against the tag before
building or publishing. They never rewrite `server.json` in the runner — that
used to hide stale metadata in the repository.

## Pre-releases (`-rc.N`)

A tag with an `-rc.N` suffix is an **internal test build**: binaries and a
container image for people who know what they are testing. It is never
published to the MCP Registry and never moves `:latest`.

Use it when a release needs real use before it is final. The release branch and
PR name the exact version being cut: `release/v0.2.8-rc.1` for `v0.2.8-rc.1`,
and `release/v0.2.8` for the final `v0.2.8`. CI derives the expected version
from that name and requires the changelog to describe exactly it, so each build
gets its own branch (`release/v0.2.8-rc.2` for the next rc).

```
make prepare-release RELEASE_VERSION=v0.2.8-rc.1
# ... edit the rc's notes, run the checks, merge the PR ...
git tag v0.2.8-rc.1 <merged commit>
```

**An rc does not touch `server.json`.** The manifest keeps naming the newest
final release, because the registry entry and `:latest` are what users install
and neither moves for an internal build. `prepare-release` therefore writes the
heading and leaves the manifest alone, and both guards below reject a manifest
that was bumped by an rc anyway.

The rc section keeps the full list of what went into it. The final release of
the same version is cut afterward from whatever Unreleased notes accumulated
(fixes found by testing the rc), so `v0.2.8` may follow `v0.2.8-rc.2` — a final
release outranks any rc of the same base version.

## What enforces this

Two guards read the same two files and must agree. They disagreeing is a real
failure mode, not a hypothetical: with a bare `\d+\.\d+\.\d+` heading pattern,
`## v0.2.8-rc.1 — ...` parses as `0.2.8` in Go and the Go guard demands exactly
the manifest bump the Python guard forbids.

- `scripts/release_metadata.py` (`make check-release`, run by all three
  workflows) — pins each heading whole, including its date, so an rc can never
  be read as the final version before it.
- `internal/mcp/version_test.go` `TestServerJSONMatchesChangelog` — the local
  twin that runs on every `go test ./...`. It compares against the newest
  **final** heading and skips pre-releases.
- `scripts/release_metadata_test.py` — self-check for the rc rule; run it after
  touching the script.
- `.github/workflows/publish-mcp-registry.yml` — builds the image under its rc
  version, and skips the registry login/publish for `-rc.` tags.
- `.github/workflows/release.yml` — marks `-rc.` tags as GitHub pre-releases.

Versions are `vMAJOR.MINOR.PATCH` or `vMAJOR.MINOR.PATCH-rc.N`. Other suffixes
(`-beta.1`, `-1`) are rejected by design: add one only with a matching rule for
what it means to the registry and to `latest`.
