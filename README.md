# radar

A Go implementation of **RADAR (Risk Aware Diff Auto Review)**, the code-review
automation funnel from *"Automating Low-Risk Code Review at Meta: RADAR, Risk
Calibration, and Review Efficiency"* ([arXiv:2605.30208](https://arxiv.org/pdf/2605.30208)).

RADAR classifies every code-review diff and decides whether it can be
auto-approved / auto-landed without human review (when it is genuinely low-risk)
or must be routed to a human reviewer. This package implements that funnel as a
small, dependency-free library plus a CLI.

## Install

Prebuilt archives for Linux, macOS, and Windows (amd64 + arm64) are attached to
every [GitHub release](https://github.com/travisjeffery/radar/releases). Each
archive contains both the `radar` and `radar-gh` commands.

### Download a release binary

Grab the archive for your platform from the
[latest release](https://github.com/travisjeffery/radar/releases/latest), or
from the command line (example: macOS Apple Silicon):

```sh
VERSION=0.1.0
OS=darwin    # darwin | linux | windows
ARCH=arm64   # amd64 | arm64
curl -sSL -o radar.tar.gz \
  "https://github.com/travisjeffery/radar/releases/download/v${VERSION}/radar_${VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf radar.tar.gz radar radar-gh
sudo mv radar radar-gh /usr/local/bin/   # or anywhere on your PATH
radar -h
```

On Windows the asset is a `.zip` instead of a `.tar.gz`.

With the [GitHub CLI](https://cli.github.com) you can download assets directly:

```sh
gh release download v0.1.0 --repo travisjeffery/radar --pattern '*darwin_arm64*'
```

### Verify the download (optional)

Each release includes `checksums.txt` (SHA-256):

```sh
gh release download v0.1.0 --repo travisjeffery/radar --pattern 'checksums.txt'
shasum -a 256 -c checksums.txt --ignore-missing
```

### Install with Go

```sh
go install github.com/travisjeffery/radar/cmd/radar@latest
go install github.com/travisjeffery/radar/cmd/radar-gh@latest
```

## The funnel

`Engine.Classify(Diff)` routes a diff through the layered funnel and returns a
`DecisionTrace` recording the final decision and every gate evaluated (so the
routing is fully auditable, mirroring Figures 2–4 of the paper).

```
                          ┌─ human ──────────► RADAR Verification → RADAR Approval
New Diff ─ authorship ─┤                        (Fig. 4: P5 DRS, ACR, stricter waiver)
                          └─ bot ─ source? ─┬─ deterministic codemod → Blanket AutoAccept
                                            ├─ AI codemod ──────────► ACE pipeline
                                            └─ RACER runbook ─ per-runbook ─► ACE pipeline
                                                              eligibility
```

**ACE pipeline** (bot diffs, Fig. 3) — three layers, all must pass:

1. **Static heuristics** — scope (no open-source/SOX/extra-review), CI/state,
   content blocklists, path blocklists.
2. **DRS threshold** — the diff's Diff Risk Score percentile must be ≤ the
   source's threshold (P50 allowlisted, P20 default).
3. **ACR** — the Automated Code Review agent requires confidence ≥ 8/10 and
   **zero** risk signals.

Pass all → **auto-land** (after a configurable delay); any fail → **route to human**.

**RADAR Verification + Approval** (human diffs, Fig. 4) — two steps: Verification
gates on eligibility + content + ACR + DRS (P5 default) and, if it passes, the
author ships with *deferred* review (`verification-passed`); Approval applies a
stricter DRS threshold and maximal ACR confidence to waive human review entirely
(`radar-approved`).

### Decisions

`blanket-auto-accept`, `auto-land`, `verification-passed`, `radar-approved`,
`route-to-human`, `not-eligible`.

## Pluggable components

The two pieces that are proprietary/ML at Meta are interfaces here:

- **`RiskScorer`** — the Diff Risk Score (DRS). `HeuristicScorer` is a
  transparent stand-in (size, complexity, risky paths, risk signals); a
  `Calibrator` maps raw scores to percentiles via an empirical CDF, so a
  threshold `PX` means "only the safest X% qualify" (paper §2.3). An engine
  built without `WithCalibrator` uses a built-in synthetic sample; an
  explicitly empty calibrator fails closed (everything routes to human).
- **`ReviewAgent`** — the Automated Code Review (ACR). `RuleBasedAgent` (default,
  offline, deterministic) classifies the paper's safe/risk signal taxonomy from
  structured change tags. `LLMAgent` (Anthropic), `OpenAIAgent` (OpenAI), and
  `FireworksAgent` (open-weight models served by Fireworks AI) are optional
  API-backed adapters behind the same interface; all fail safe (route to human)
  on any error. Every agent reports a provenance string (`rule-based`,
  `openai/<model>`, `anthropic/<model>`, `fireworks/<model>`) that
  `PullRequestReview` records as `reviewer`.

Per-org risk appetite (`OrgPolicyConfig`, modeling `OrgRADARPolicyConfig`) and
per-runbook eligibility (60-day risk history, daily limits, denylist) are
configurable, matching Table 1.

## CLI

If you installed the release binaries, invoke `radar` and `radar-gh` directly.
From a source checkout, use `go run ./cmd/radar` in place of `radar`.

```sh
radar classify [-llm] [-json] testdata/human_approved.json
radar replay   [-llm]         testdata/diffs.json
```

`classify` prints the decision and the stage-by-stage trace for one diff.
`replay` streams a batch and reports the paper's RQ metrics (reviewed/landed
counts, approve rate, verification pass rate, coverage, per-source breakdown).
With `-llm` the ACR uses the Anthropic API (`$ANTHROPIC_API_KEY`,
optional `$RADAR_ACR_MODEL`); otherwise the deterministic rule-based ACR is used.

See `testdata/` for fixtures exercising every decision path.

### Running against real GitHub PRs

`cmd/radar-gh` pulls real pull requests (via the `gh` CLI) and runs them through
the funnel — a way to exercise the LLM ACR against real diffs:

```
radar-gh -repo OWNER/REPO -limit 15 -llm [-human-drs 5] [-state all]
```

PRs are treated as eligible human-authored diffs (GitHub PRs lack Meta's
author-eligibility attributes), so the decision turns on content risk: the LLM
ACR classifies each diff and the DRS threshold gates on calibrated risk. CI and
lifecycle state are taken from the PR itself — failing or pending checks fail
the state gate, and PRs closed without merging count as rejected.
`-human-drs` overrides the human DRS percentile threshold (paper default P5) for
exploration. With `-llm` the ACR uses the OpenAI API (`$OPENAI_API_KEY`,
optional `$RADAR_ACR_MODEL`, default `gpt-4o-mini`). The OpenAI adapter uses the
Responses API with strict structured output and explicitly disables response
storage.

### Risk-aware GitHub review automation

`radar-gh review` is the production-oriented, provider adapter. It evaluates one
exact pull request head against a versioned policy and emits a structured JSON
decision:

```sh
radar-gh review \
  -repo OWNER/REPOSITORY \
  -pr 123 \
  -policy .github/radar-policy.json \
  -expected-head "$HEAD_SHA" \
  -agent openai      # or anthropic, fireworks, rule-based; or a comma list
```

The default example policy is `shadow`: a qualifying change produces
`would-approve`, but Radar does not write to GitHub. Other outcomes are
`route-to-human` or `policy-update-candidate`; Radar never merges a pull request
and never submits `REQUEST_CHANGES`.

The adapter fails closed unless it can prove all of the following from complete,
paginated GitHub state:

- the PR is open, ready, from the same repository, and targets an allowed base;
- checks exist, are passing, and have the same fingerprint across two reads;
- every changed file has a complete patch and one allow rule covers the whole PR;
- no denied path or phrase matches either side of a rename;
- no review requests changes and no review thread is unresolved;
- the calibrated risk threshold and strict LLM review both pass.

The review gate needs the model's own accept claim, confidence of at least
`min_review_confidence`, no risk signals, at least one safe signal, full file
coverage, and no finding whose severity is in `blocking_finding_severities`
(default `["P0","P1","P2"]`; it must always include P0 and P1). A policy that
sets `["P0","P1"]` lets a change with only P2/P3 findings through; the decision
records `model_accept` and the severities applied.

The LLM can veto an allowlisted change. A strong safe verdict outside the
deterministic allowlist is only reported as a `policy-update-candidate`; it
cannot approve the PR. Copy [the generic policy](examples/github-policy.json)
and [GitHub Actions workflow](examples/github-actions/radar-review.yml) to start
in shadow mode. Replace the illustrative calibration sample with scores from
your own merged-PR history before considering approval mode.

#### Deciding from the review findings

A policy with `"approval_basis": "review-findings"` drops the allow rules, the
global file and line limits, and the risk threshold (the heuristic score grows
with line count, so it is recorded but does not gate). It must not set them, so
a policy never looks as if a limit applies when it does not. A reviewer passes
when it covered every file it was shown, reported confidence of at least
`min_review_confidence`, raised no finding in `blocking_finding_severities`,
explained any decline with a recorded signal, and raised no defect signal (`bug-or-logic-error`, `performance-risk`,
`secrets-exposure`, `sql-injection`, `auth-bypass`). `structural-change` and
`high-review-effort` describe a change's size and shape, so they are recorded
but do not block. The state gate, deny paths and deny phrases still apply.

`required_reviewers` (default 1) is how many review agents must run; every one
must pass. Pass several to `-agent`, each with its own model:

```sh
radar-gh review ... -agent openai,fireworks=accounts/fireworks/models/glm-5p3
```

With several agents `$RADAR_ACR_MODEL` must be unset. The agents review
concurrently from one snapshot; the decision records each verdict in `reviews`
and their conservative merge in `agent`.

A diff larger than `review_chunk_chars` (default 240000, about 60k tokens) is
reviewed in several requests, each holding whole files and told which part it
is; the parts' verdicts merge conservatively. A single file over the budget is
not truncated: that reviewer fails safe and says why.

#### Generated files

`generated_files` withholds generated files from the review agents, which see
only their path, status and line counts. Paths come from `generated_files.paths`
and from the `linguist-generated` entries of the root `.gitattributes`
(`generated_files.gitattributes: ".gitattributes"`), read from the trusted
checkout the policy is read from and never from the pull request head, so a
change cannot mark its own files generated. As in git, the last matching
`.gitattributes` line decides. Only a file that already existed at a generated
path is withheld: a file the change adds there, or renames in from a
hand-written path, is reviewed in full, so naming a new file like generated code
cannot hide it. `header_markers` (regular expressions for headers such as
`Code generated ... DO NOT EDIT.` or `@generated`) never withhold anything,
since anyone can write a header; a reviewed file that carries one outside every
generated path is listed in `generated_unlisted`, so its generator path can be
added. Withheld files still count for deny paths and phrases, and a pull request
whose every file is withheld routes to a human. `generated_files.note` is passed
to the agents, for example to say which CI checks verify generated output. The
decision lists what was withheld in `withheld`.

#### Lockfiles

With a `lockfiles` block, recognised lockfiles (`gradle.lockfile`,
`poetry.lock`, `uv.lock`, `go.sum`, `package-lock.json`) are never withheld as
generated. Each agent gets a summary parsed from the patches (per package: old
and new version, added or removed, and changes to where it is resolved from or
to an integrity hash without a version change) followed by the raw lines. A
lockfile whose +/- lines and summary repeat an earlier one's, as when one bump
regenerates many Gradle lockfiles, is shown as a pointer to the first; one too
large for a request is split by hunk across parts instead of failing. The
prompt makes supply-chain signals P1 findings: typosquats, new or changed
sources, downgrades, hash-only changes, and lockfile changes without a matching
manifest change. Two deterministic backstops route to a human:
`require_human_without_manifest` (no `pyproject.toml`, `go.mod` or
`package.json` change beside the lockfile; for Gradle, no build script, version
catalog or `gradle.properties` change anywhere) and
`require_human_on_source_change` (any registry, index, git or URL source
change). The decision records the summaries in `lockfiles`.

#### Stacked pull requests

With `allow_stacked`, a pull request whose base is another open
same-repository pull request's head branch is evaluated when the chain of open
pull requests below it (at most 8) ends on an allowed base. The diff is GitHub's
diff against the pull request's own base, so the verdict covers its own changes
only, and `stack` records the pull requests below it. A stacked pull request is
never approved, even in approve mode: it stops at `would-approve`, because an
approval would survive a retarget to `main` that brings the unreviewed changes
below it into the diff. Once it targets an allowed base it is evaluated like any
other pull request. A base branch that heads more than one open pull request is
treated as unstacked.

Approval is an explicit second rollout. Change the policy mode to `approve`,
enable GitHub's branch-protection setting that dismisses stale approvals, and
pass both `-apply` and the event's `-expected-head`. Radar then re-reads the
head, checks, threads, reviews, and branch protection immediately before posting
an approval bound to that commit. The initial shadow rollout intentionally does
not require or enable stale-approval dismissal.

The `openai` agent uses `$OPENAI_API_KEY`; `anthropic` uses
`$ANTHROPIC_API_KEY`; `fireworks` uses `$FIREWORKS_API_KEY` **and requires**
`$RADAR_ACR_MODEL` set to a full Fireworks model id (for example
`accounts/fireworks/models/glm-5p3` or `accounts/fireworks/models/kimi-k2p7-code`).
There is deliberately no default Fireworks model: the platform serves many
interchangeable open-weight models, and the one doing the reviewing should
change only through an explicit configuration change. The adapter calls the
OpenAI-compatible Chat Completions endpoint with `temperature: 0` and a
`json_schema` response format, and fails safe when the response is truncated at
`max_tokens` (`$RADAR_ACR_MAX_TOKENS`, default 16384 to leave room for reasoning
models), when a review exceeds its deadline (`$RADAR_ACR_TIMEOUT`, a Go
duration, default `240s`; each verdict records `elapsed_ms`), when the served `model` differs from the requested one, or when the
verdict does not parse. `$FIREWORKS_BASE_URL` points it at another
OpenAI-compatible Chat Completions endpoint. Use a dedicated GitHub App or service account token with
read access to repository contents, checks, pull requests, and review threads.
Approval mode additionally needs branch-protection read access and pull-request
review write access.

## Scope / non-goals

- This reproduces RADAR's **decision logic and metric definitions**, not Meta's
  empirical telemetry (the 535K+ diffs, revert/PI rates — those are
  observational and not reproducible).
- `HeuristicScorer` is a faithful *interface* stand-in for Meta's proprietary DRS
  model, not a retrained equivalent.
- Zero third-party dependencies; the LLM adapter uses only `net/http` and
  `encoding/json`.

## Tests

```
go test ./...
```
