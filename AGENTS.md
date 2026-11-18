# hydra — agent guide

> **Read first:** this repo is the **`hydra` CLI** — a Go command-line tool that installs and
> manages on-prem Kubernetes clusters through Cluster API. It is bound by **ADR-001** (Notion,
> *Hydra CLI is a stateless orchestrator, not a control plane*, Accepted 2026-11-14): the CLI
> keeps **no state of its own**. Read the ADR before adding a command.
>
> PR titles are `[PET-xx] Title`. Never put AI attribution in commits or PR bodies, but **do**
> prefix every PR comment you write with `**<Agent> (<Model>)**`. See
> [Working agreements](#working-agreements).

## What this repo is

The user-facing CLI for Project Hydra. It reads one input, `hydra.yaml`
(`apiVersion: hydra.io/v1alpha1`, `kind: ClusterConfig`), and drives host tooling, kind, clusterctl
and the Kubernetes API. The infrastructure provider it installs lives in
`cluster-api-provider-hydra`; this repo consumes the provider's released artifacts and never imports
its controller code.

Commands, in delivery order (Linear project *Hydra — Reliability & Productization*):

| Command | Ticket | State |
|---|---|---|
| `hydra cluster render` | PET-78 | implemented |
| `hydra validate` | PET-79 | implemented |
| `hydra bootstrap` / `bootstrap delete` / `bootstrap kubeconfig` | PET-81 | implemented |
| `hydra cluster create` / `plan` / `status` / `delete` | PET-82 | implemented |
| `hydra cluster pivot` / `create --self-hosted` | PET-83 | implemented |
| `hydra cluster delete --self-hosted`, `--finish-split` | PET-87 | implemented, proven on hardware |
| `hydra cluster upgrade` | PET-24 | implemented (hardware proof pending) |

## Layout

| Path | What |
|---|---|
| `cmd/hydra/` | `main` — wiring only |
| `internal/cli/` | cobra command tree |
| `internal/config/` | `hydra.yaml` types, defaults and validation (`Parse` is the only entry) |
| `internal/render/` | `hydra.yaml` → Cluster API manifests; `templates/cluster.yaml.tmpl` |
| `internal/cluster/` | create/plan/status/delete against a management cluster: SSA apply, live dry-run diff, controller-reported status, label-scoped teardown, safety checks |
| `internal/bootstrap/` | the temporary kind management cluster: kind and clusterctl as Go libraries behind small interfaces, faked in tests |
| `internal/validate/` | read-only host and hypervisor checks behind small interfaces (libvirt over TLS, TCP probes from this host or the kind node), faked in tests |
| `internal/pivot/` | moving a cluster's management onto itself and back for teardown: locate, preflight, `clusterctl move`, verify, split evidence; a teardown record in kind (never moves back or unpauses on its own after a pivot) |
| `internal/render/assets/cilium.yaml` | **generated** by `hack/gen-cilium.sh`; never edit by hand |
| `internal/render/testdata/*.golden.yaml` | expected output for each file in `examples/` |
| `examples/` | user-facing `hydra.yaml` examples; every one is golden-tested |

## Rules specific to this repo

1. **No CLI-local state** (ADR-001). No database, cache, inventory, progress ledger or resume
   checkpoint. An interrupted command must be retryable by rediscovering state from kind/Docker, the
   Kubernetes API and clusterctl. Allowed persistence is exactly: the kubeconfig, an optional
   defaults-only `~/.config/hydra/config.yaml`, explicitly requested exports, and the temporary kind
   cluster itself (found again by name).
2. **`render` is deterministic.** No clock, randomness, map-order or network input. Changing output is
   a reviewed change: run `make golden`, and commit the regenerated golden files with the code.
3. **Never render secrets.** Rendered manifests are shareable output. Credentials Secrets are
   referenced by name and created by commands that read them from files (PET-81/82).
4. **The templates descend from proven manifests.** `cluster.yaml.tmpl` is derived from the
   provider's `docs/examples/`, which were verified on hardware. When you change a setting there,
   find the provider ticket that proved it, and keep the two in step.
5. **Pinned versions are code.** Kubernetes default, Cilium (`CILIUM_VERSION` in
   `hack/gen-cilium.sh` and `render.CiliumVersion`), kube-vip, and bootstrap's kind node image,
   cert-manager, Cluster API and Hydra provider (`internal/bootstrap`) change only in a reviewed PR.
   The kind node image is pinned by digest.
6. **Generic over lab.** No default may encode the reference lab (its bridge, pool, addresses).
   Lab values belong in `examples/` only, marked as such.

## Development

```bash
make test          # unit + golden tests
make golden        # regenerate golden files after an intended output change
make lint          # golangci-lint, built with the pinned toolchain (see below)
make verify        # go.mod tidy + gofmt
make cilium        # regenerate the embedded Cilium manifest (needs helm)
make e2e           # kind management cluster via `hydra bootstrap`, dry-run every example (needs Docker)
make vuln          # govulncheck
make actionlint    # lint .github/workflows
make dist          # release binaries + checksums into dist/
```

CI (`.github/workflows/`): `ci.yml` (verify, test, build, lint, govulncheck, actionlint) on every PR;
`e2e.yml` (the kind e2e) on PRs touching code or examples and on main; `cilium-drift.yml` weekly
and on renderer changes; `release.yml` publishes binaries on a `v*` tag (manual runs build an
artifact only). Every job runs on GitHub-hosted runners. Actions are pinned by commit SHA;
Dependabot keeps them and the Go modules current.

**The Go toolchain is pinned in `go.mod` (`toolchain go1.26.x`).** `actions/setup-go` installs it, so
CI and release binaries build with the latest patch, not the bare `go 1.26.0` floor; on 1.26.0
govulncheck finds 30 reachable standard-library vulnerabilities. Bump the patch when govulncheck
reports a standard-library fix. A locally installed Go that is newer than the pin (1.27) wins over
the `toolchain` line, and golangci-lint's typecheck currently breaks on 1.27; run
`GOTOOLCHAIN=go1.26.11 make lint` (the pinned version, never an older patch) until it is fixed.

## Project context

**Hydra** is an open-source on-prem Kubernetes lifecycle platform built on
[Cluster API](https://cluster-api.sigs.k8s.io/), with libvirt/KVM as the first infrastructure
provider and workload-driven node autoscaling via Cluster Autoscaler. It lives on the
[`Petatron`](https://github.com/Petatron) GitHub org across five repositories:

| Repo | Role |
|---|---|
| [`hydra`](https://github.com/Petatron/hydra) | The **`hydra` CLI** (Go) — stateless orchestrator over Cluster API (ADR-001). |
| [`cluster-api-provider-hydra`](https://github.com/Petatron/cluster-api-provider-hydra) | The Cluster API **infrastructure provider**. Go, kubebuilder. This is where product code lives. |
| [`hydra-bootstrap`](https://github.com/Petatron/hydra-bootstrap) | **Documentation only.** Runbooks for standing up hosts, the CAPI management cluster, and workload clusters. |
| [`hydra-gitops`](https://github.com/Petatron/hydra-gitops) | Argo CD configuration. **A merge to `main` changes live clusters.** |
| [`hydra-infra`](https://github.com/Petatron/hydra-infra) | Terraform for the pre-Hydra, hand-built worker VMs. Legacy path, being superseded by the provider. |

**Where truth lives — three systems, deliberately separated:**

- **Notion** (PETATRON teamspace → *Project Hydra*) — architecture, ADRs, design docs, runbooks,
  worklogs, validation evidence. This is the source of truth for **engineering knowledge**.
- **Linear** (workspace `petatron`, team key `PET`) — issues `PET-5`..`PET-46+`. Source of truth
  for **delivery status**. `PET-1`..`PET-4` are onboarding stubs, not real work.
- **GitHub** — code, PRs, CI. Not a place to record decisions.

Durable findings go in Notion, **not** Linear comments. Every Linear issue must have a matching
Notion page, linked both ways.

## Working agreements

These are instructions, not suggestions.

### 1. PR titles carry the ticket; commit messages do not

```
PR title:  [PET-27] Publish template capacity for scale-from-zero
Commit:    feat: publish template capacity for scale-from-zero
```

The ticket goes in **square brackets at the front** of the PR title. Commit messages stay in
conventional style (`feat:` / `fix:` / `docs:` / `chore:` / `ci:`) with **no** ticket prefix.
`feat: … (PET-27)` is the wrong shape for a PR title — do not copy it.

**Every PR needs a ticket. If there isn't one, stop and ask.** When the work has no Linear (or Jira)
issue, do not open the PR on your own judgment — ask whether to create a ticket first or to open
this one without a prefix, and let the user decide. **Never invent or guess a number:** a wrong
`[PET-xx]` silently attaches the PR to somebody else's work and corrupts the tracking both systems
exist to provide.

### 2. Never put AI attribution in git history

Commit messages, commit trailers, PR titles, and PR bodies must **never** mention Claude, Codex,
Cursor, Copilot, or any AI assistant. Git history records what changed and why, not which tool
typed it. Never emit:

```
Co-Authored-By: Claude <noreply@anthropic.com>
Co-authored-by: Cursor <cursoragent@cursor.com>
🤖 Generated with [Claude Code](https://claude.com/claude-code)
Made with [Cursor](https://cursor.com)
```

Nor phrases like "AI-assisted", "generated by AI", or a model name anywhere in a commit message or
PR description. Write them as the human author would.

**Some tools inject attribution after the fact — always verify.** Writing a clean message is not
enough:

```bash
ATTRIB='^Co-authored-by:|Generated with \[|Generated with Claude|Made with \[|AI-assisted|generated by AI|noreply@anthropic\.com|cursoragent@cursor\.com|🤖'
git cat-file -p HEAD | grep -iE "$ATTRIB" && echo DIRTY || echo clean
gh pr view <n> --json body --jq '.body' | grep -iE "$ATTRIB" && echo DIRTY || echo clean
```

Match the *attribution strings*, **not** product names. Bare `claude`/`cursor` hit the filename
`CLAUDE.md`; bare `Claude Code` hits any commit that legitimately mentions the tool. Both report
clean commits as dirty.

`git commit --amend` re-injects the trailer and cannot fix this; rebuild the commit object with
`git commit-tree` instead. If the bad commit was already pushed, **ask before force-pushing**, then
use `--force-with-lease=<branch>:<old-sha>`.

### 3. DO attribute yourself in PR comments

This is the exception to rule 2, and it is required. Multiple agents (and the human maintainer)
review the same PRs; the comment thread has to say who is speaking.

**Prefix every GitHub PR comment, review comment, review summary, and reply you write with your
agent name and model in bold:**

```
**Claude (Opus 5)** — the informer starts on the first cached Get, so declining
the Watch bought nothing here. Read the Secret through mgr.GetAPIReader().
```

```
**Codex (GPT-5)** — agreed, but the RBAC verb also needs narrowing to `get`.
```

Format is `**<Agent> (<Model>)**` followed by an em dash. Use the name a reader would recognise —
`Claude (Opus 5)`, `Codex (GPT-5)`, `Cursor (Composer)`. If you genuinely do not know your model
identity, use `**<Agent>**` alone rather than guessing.

This applies to **comments only** — the conversation surface. It does **not** apply to the PR body,
the PR title, or commit messages, which stay clean under rule 2.

GitHub still attributes the comment to whichever account is authenticated. The prefix says which
agent wrote the text; do **not** claim the displayed GitHub author has changed.

### 4. Keep the record current continuously, not at the end

Whenever a ticket, a test, or a meaningful piece of progress completes, update **both**:

- **Linear** — status, plus a comment stating what was proven and, just as importantly, what was
  **not**.
- **Notion** — the linked page's worklog section and its Doc Status property.

Write down what was *learned* — especially anything that turned out different from what was
assumed — not just what shipped. The goal is that a future session resumes with no lost context.

Every operation performed against real infrastructure gets recorded as it happens, in both.

### 5. Read PR review comments via GraphQL, never REST

The REST endpoint `/pulls/N/comments` **silently omits threads** — it missed all eight of the
maintainer's review comments on one PR. Always use `reviewThreads`:

```bash
gh api graphql -f query='{repository(owner:"Petatron",name:"<repo>"){pullRequest(number:N){
  reviewThreads(last:80){nodes{isResolved path line
    comments(first:1){nodes{author{login} createdAt body}}}}}}}'
```

Filter by `createdAt` to find new threads. Do not slice by index against the REST count.

### 6. Expect several review rounds, and verify every comment

Copilot reviews every PR and has found dozens of real defects — several that would only have
surfaced in production as orphaned VMs or wedged finalizers. **Expect 3–5 review rounds per PR.**

Verify each comment against the actual file before acting on it. Some have been stale test
assumptions rather than code bugs, and at least one was a linter firing on a conversion that was
genuinely required. A review comment is evidence, not a verdict.

### 7. Generic design beats lab convenience

The reference lab (an XPS 13 control plane at home, a workstation at the office, ~30 ms apart) is
one person's setup, **not a requirement Hydra should be shaped around**. Hydra must assume nothing
about that hardware, topology, or site count.

When lab convenience and generic design diverge, **generic wins** — unless the shortcut is provably
free, meaning a config value that can change later, not a code path that would have to be undone.

Architecture decisions carry a **Scope** property in Notion: `Product` binds Hydra for everyone,
`Reference lab` describes only that setup. Do not read a lab ADR as a product constraint.
