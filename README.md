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

Both routes produce a self-contained binary: the trained DRS model artifact
(`drs_model.json`, see below) is compiled in with `go:embed`, so a release
archive pins the model version along with the code.

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

- **`RiskScorer`** — the Diff Risk Score (DRS). Two implementations are
  selectable by name (`radar.NewRiskScorer`, the CLIs' `-scorer` flag):
  `heuristic` (`HeuristicScorer`, the default: a transparent stand-in over
  size, complexity, risky paths, risk signals) and `drs-model`
  (`DRSModelScorer`, a trained linear model over diff features — see
  [Trained DRS model](#trained-drs-model--scorer-drs-model)). A `Calibrator`
  maps raw scores to percentiles via an empirical CDF, so a threshold `PX`
  means "only the safest X% qualify" (paper §2.3). The calibration sample is
  specific to the scorer that produced it. An engine built without
  `WithCalibrator` uses a built-in synthetic sample on the heuristic scorer's
  scale; an explicitly empty calibrator fails closed (everything routes to
  human).
- **`ReviewAgent`** — the Automated Code Review (ACR). `RuleBasedAgent` (default,
  offline, deterministic) classifies the paper's safe/risk signal taxonomy from
  structured change tags. `LLMAgent` (Anthropic) and `OpenAIAgent` (OpenAI) are
  optional API-backed adapters behind the same interface; both fail safe (route
  to human) on any error.

Per-org risk appetite (`OrgPolicyConfig`, modeling `OrgRADARPolicyConfig`) and
per-runbook eligibility (60-day risk history, daily limits, denylist) are
configurable, matching Table 1.

## CLI

If you installed the release binaries, invoke `radar` and `radar-gh` directly.
From a source checkout, use `go run ./cmd/radar` in place of `radar`.

```sh
radar classify  [-llm] [-json] [-scorer NAME] [-calibration sample.json] testdata/human_approved.json
radar replay    [-llm] [-scorer NAME]  testdata/diffs.json
radar calibrate [-scorer NAME] [-points 41] testdata/diffs.json
```

`classify` prints the decision and the stage-by-stage trace for one diff.
`replay` streams a batch and reports the paper's RQ metrics (reviewed/landed
counts, approve rate, verification pass rate, coverage, per-source breakdown).
With `-llm` the ACR uses the Anthropic API (`$ANTHROPIC_API_KEY`,
optional `$RADAR_ACR_MODEL`); otherwise the deterministic rule-based ACR is used.

`-scorer` selects the DRS implementation (`heuristic`, the default, or
`drs-model`). `replay` calibrates against the batch it is given, so it works
with either scorer. `classify` calibrates a single diff against the built-in
synthetic sample, which is on the heuristic scale; with `-scorer drs-model` it
requires `-calibration sample.json` — a JSON array of raw scores as printed by
`radar calibrate -scorer drs-model` — and refuses to run without one.

`calibrate` scores every diff in the input with the selected scorer and prints
an ascending quantile sample to stdout, ready to paste into a policy's
`calibration_sample` (with a one-line n/min/median/p90/max summary on stderr).
For `-points N` and `n` sorted scores it takes, for `k` in `0..N-1`, the value
at index `round(k*(n-1)/(N-1))`, rounding half away from zero (Go's
`math.Round`; a Python port must not use the built-in round-half-to-even
`round()`). The first and last points are therefore the min and max. Set the
policy's `calibrated_for` to the same scorer name.

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
  -agent openai \
  -scorer heuristic
```

`-scorer` defaults to `heuristic`. Because a calibration sample only has
meaning for the scorer that produced it, the policy declares which one with
`calibrated_for` (`heuristic` or `drs-model`; omitted means `heuristic`, which
is how every policy written before the field existed was calibrated). If
`calibrated_for` disagrees with `-scorer`, `radar-gh review` exits 1 before
making any GitHub call. The emitted decision records the scorer in the
`pr.risk-threshold` stage reason and, for a model scorer, its
`model_version`, so the retained JSON artifact shows exactly what produced the
percentile.

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

The LLM can veto an allowlisted change. A strong safe verdict outside the
deterministic allowlist is only reported as a `policy-update-candidate`; it
cannot approve the PR. Copy [the generic policy](examples/github-policy.json)
and [GitHub Actions workflow](examples/github-actions/radar-review.yml) to start
in shadow mode. Replace the illustrative calibration sample with scores from
your own merged-PR history before considering approval mode.

Approval is an explicit second rollout. Change the policy mode to `approve`,
enable GitHub's branch-protection setting that dismisses stale approvals, and
pass both `-apply` and the event's `-expected-head`. Radar then re-reads the
head, checks, threads, reviews, and branch protection immediately before posting
an approval bound to that commit. The initial shadow rollout intentionally does
not require or enable stale-approval dismissal.

The `openai` agent uses `$OPENAI_API_KEY`; `anthropic` uses
`$ANTHROPIC_API_KEY`. Use a dedicated GitHub App or service account token with
read access to repository contents, checks, pull requests, and review threads.
Approval mode additionally needs branch-protection read access and pull-request
review write access.

## Trained DRS model (`-scorer drs-model`)

`DRSModelScorer` is a logistic-regression-style linear model over named diff
features. The artifact is JSON, embedded in the binary from `drs_model.json`
at the repository root:

```json
{
  "schema_version": 1,
  "model_version": "drs-lr-2026-09-08",
  "trained_at": "2026-09-08T00:00:00Z",
  "training_run": "optional free-form provenance string",
  "intercept": -1.8,
  "features": [
    {"name": "lines_changed", "transform": "log1p", "mean": 3.1, "scale": 1.2, "coefficient": 0.9},
    {"name": "risky_path_files", "transform": "identity", "mean": 0.0, "scale": 1.0, "coefficient": 1.4}
  ]
}
```

The raw score is the logit
`intercept + Σ coefficient_i * ((transform_i(x_i) - mean_i) / scale_i)`;
it is monotone in risk, which is all the `Calibrator` needs. Supported
transforms are `identity` and `log1p`.

The split of responsibilities is deliberate: the training pipeline (a
separate Python package) decides *which* features to use, in what order, and
with what weights; this package is the source of truth for *how* each named
feature is computed from a `Diff`. The registry (`radar.DRSModelFeatureNames()`,
pinned by `TestDRSModelFeatureNames`) is:

| feature | rule |
| --- | --- |
| `file_count` | number of changed files |
| `additions` / `deletions` | sums of provider-reported line counts |
| `lines_changed` | `Diff.LinesChanged()`: additions+deletions per file, falling back to newlines+1 of the patch when both are zero |
| `max_file_lines` | the largest single file under the `lines_changed` rule |
| `risky_path_files` | files whose lowercased path contains any `HeuristicScorer` risky fragment (`auth`, `crypto`, `secret`, `payment`, `billing`, `migration`, `schema`, `security`, `prod`, `config`), counted once per file |
| `added_files` / `removed_files` / `modified_files` / `renamed_files` | files by change type (case-insensitive) |
| `test_files` | any directory segment `test`/`tests` (case-insensitive), or a file name matching `*_test.*`, `test_*`, `*.test.*`, `*.spec.*`, or a stem ending in `Test`/`Tests` (case-sensitive) |
| `doc_files` | lowercased path ends in `.md` or has a `docs` directory segment |
| `config_files` | lowercased path ends in `.yaml`, `.yml`, `.json`, `.toml`, `.properties`, `.gradle`, or `.kts` |
| `hunk_count` | lines beginning `@@` across all patches |

Every feature is derived from the fields the production adapter populates on
each change — path, previous path, change type, additions, deletions, and the
unified patch. `Complexity` and `Signals` are never set on that path, so no
feature uses them.

Loading fails closed. `NewDRSModelScorer` returns an error (and `-scorer
drs-model` exits non-zero) for an unknown feature or transform, a duplicate
feature, a non-positive scale, any non-finite parameter, an empty feature
list, an unsupported `schema_version`, an unknown field, an empty
`model_version`, or a `"placeholder": true` artifact. `Score` itself never
returns NaN or Inf. The committed `drs_model.json` **is** the placeholder —
`{"schema_version":1,"placeholder":true,...}` — so `-scorer drs-model` is
refused until a trained artifact replaces it. The reviewer additionally treats
any non-finite raw score as a failed risk gate.

Rolling out a model is therefore: replace `drs_model.json`, cut a tagged
release (the goreleaser archive now embeds it), have the consuming workflow
bump its pinned release version and checksum, publish a policy with
`calibrated_for: "drs-model"` and a `calibration_sample` produced by
`radar calibrate -scorer drs-model` over that repository's merged-PR history,
and pass `-scorer drs-model`. Nothing changes for existing policies until that
last step.

## Scope / non-goals

- This reproduces RADAR's **decision logic and metric definitions**, not Meta's
  empirical telemetry (the 535K+ diffs, revert/PI rates — those are
  observational and not reproducible).
- `HeuristicScorer` is a faithful *interface* stand-in for Meta's proprietary DRS
  model, not a retrained equivalent. `DRSModelScorer` only evaluates a model;
  training lives in a separate pipeline.
- Zero third-party dependencies; the LLM adapter uses only `net/http` and
  `encoding/json`.

## Tests

```
go test ./...
```

CI (`.github/workflows/go-test.yml`) runs `gofmt -l`, `go vet ./...`, and
`go test ./...` on every pull request and push to `main`. The suite is fully
offline; model tests use synthetic fixtures under `testdata/drs_model/`, never
trained weights.
