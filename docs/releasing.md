# Releasing the hydra CLI

**A release is a PR that changes `VERSION`** (PET-92). The file at the repository root holds the
version `main` is at, without the `v`, e.g. `0.3.0`. When a commit reaches `main` with a `VERSION`
that has no tag yet (normally the bump's merge commit; a later one if that run was queued behind
another and superseded), `.github/workflows/release.yml`:

1. re-runs `make verify test lint vuln`, and refuses to publish if any fail;
2. builds `hydra-{linux,darwin}-{amd64,arm64}` and `checksums.txt` with `make dist VERSION=vX.Y.Z`,
   and checks that the linux/amd64 binary reports `hydra vX.Y.Z`;
3. creates the annotated tag `vX.Y.Z` on that commit, then the GitHub Release `vX.Y.Z` with the
   binaries and checksums attached. A version with a `-` (`0.4.0-rc.1`) is marked a pre-release.

Any other push to `main` stops after the version job. A manual run (`workflow_dispatch`) builds the
binaries as a workflow artifact and never publishes.

## Cutting a release

```bash
git switch -c pet-NN/release-vX.Y.Z origin/main
make bump-patch      # fixes only
make bump-minor      # new behaviour, new commands or flags
make bump-major      # breaking changes to the CLI or hydra.yaml, once past v1
git add VERSION && git commit -m "chore: release vX.Y.Z"
```

Open the PR as usual (`[PET-NN] Release vX.Y.Z`). The bump can also ride in the PR that carries the
change. Its **`VERSION is releasable`** check (`make check-version`, in `ci.yml`) fails when
`VERSION` is malformed or behind the newest `v*` tag; its notice says which tag the merge creates.

A pre-release is written by hand: put `0.4.0-rc.1` in `VERSION`. `make bump-*` drops the `-rc.N`
suffix while it bumps, so to finalise `0.4.0-rc.2` write `0.4.0` by hand too.

Merging releases. Runs on `main` are serialised, and a tag the workflow creates with `GITHUB_TOKEN`
does not start another run, so a version cannot be released twice. If a run fails, see
[Recovering a release](#recovering-a-release).

## Before merging a bump

- **Main is green**, including the e2e run.
- **The pinned provider is the one you mean.** `HydraVersion` / `MinHydraVersion` in
  `internal/bootstrap` name the provider release `hydra bootstrap` installs; a provider release does
  not change them. Bump them in a reviewed PR first if this release should ship a newer provider.
- **The README's "latest release" line** is updated in the same PR.

## Fallback: tagging by hand

Pushing a `v*` tag still runs the same release, for when the `VERSION` path is broken:

```bash
git fetch origin
git tag -a vX.Y.Z -m vX.Y.Z origin/main
git push origin vX.Y.Z
```

Then set `VERSION` to that version in a PR: until then it is behind the newest tag and every PR's
`VERSION is releasable` check fails. Never hand-tag a version `VERSION` already names but has not
released yet: the `main` run would build it from another commit and then refuse at its tag step.

## Recovering a release

The tag is the lock: once `vX.Y.Z` names a commit, only that commit can produce the release.

- **The run failed after the tag was created.** Re-run it, with either "Re-run failed jobs" or
  "Re-run all jobs". The version job sees the tag on this commit with no Release and finishes
  the release. If a Release already exists, its assets are re-uploaded.
- **The tag names a commit that can never build**, or a newer commit's run warns
  "`vX.Y.Z` has no GitHub Release and names another commit". Delete the tag and let the next
  `main` run release from its own commit, or bump `VERSION` again:

  ```bash
  git push origin :refs/tags/vX.Y.Z
  ```

- **The run failed before the tag was created.** Fix it forward. The next green `main` run whose
  `VERSION` is still untagged releases from that commit.

## After releasing

```bash
gh release view vX.Y.Z -R Petatron/hydra --json assets --jq '.assets[].name'
v=vX.Y.Z; curl -fsSL "https://github.com/Petatron/hydra/releases/download/$v/hydra-darwin-arm64" -o /tmp/hydra \
  && chmod +x /tmp/hydra && /tmp/hydra version
```
