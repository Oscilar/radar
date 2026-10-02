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
accepted (a decline blocks, whatever its findings or confidence; see
DECISIONS.md), and raised no defect signal (`bug-or-logic-error`, `performance-risk`,
`secrets-exposure`, `sql-injection`, `auth-bypass`). `structural-change` and
`high-review-effort` describe a change's size and shape, so they are recorded
but do not block. The state gate, deny paths and deny phrases still apply.

`required_reviewers` (default 1) is how many review agents must run; every one
must pass. `reviewer_min_confidence` lowers the confidence bar for one
reviewer, keyed by its provenance (for example
`{"bedrock/us.anthropic.claude-sonnet-5": 7}`), by at most one point. The rest
of the bar still applies, so a reviewer passing at its floor has raised no
blocking finding and no defect signal. Pass several to `-agent`, each with its own model:

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

`-agent bedrock=<inference profile>` reviews with Claude on Amazon Bedrock
(package `bedrock`, kept out of the core library so it stays dependency-free).
It uses the AWS default credential chain and `$AWS_REGION`, calls InvokeModel
through an inference profile such as `us.anthropic.claude-sonnet-5` with
adaptive thinking (`$RADAR_BEDROCK_EFFORT`, default `high`; output cap
`$RADAR_BEDROCK_MAX_TOKENS`, default 64000), and takes the verdict as the input
of a `submit_verdict` tool, because Bedrock rejects `output_config.format` and
strict tools for Claude. A response cut off at the output cap is marked
`truncated`; Radar then reviews that part again in halves, up to three times,
and a verdict that is still truncated never passes. Every verdict records its
token `usage`.

`-agent bedrock-openai=<model>` reviews with an OpenAI model on Bedrock's
Mantle endpoint, such as `openai.gpt-5.6-terra`: the Responses API with the
shared prompt and the strict verdict schema (Mantle accepts it for OpenAI
models), signed with SigV4 for `bedrock-mantle` from the AWS default credential
chain and `$AWS_REGION`. Reasoning effort is `$RADAR_CODEX_EFFORT` (default
`high`) and the output cap `$RADAR_CODEX_MAX_TOKENS` (default 32000). It sends
`prompt_cache_options: {mode: explicit}` with no breakpoint: Radar's shared
prefix is too short to be read back, so the default implicit mode would only
bill cache writes, at 1.25x input, on every review.

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

#### Review criteria

A policy's `review_criteria` names a rules file in the trusted checkout the
policy is read from (`"review_criteria": {"rules": ".radar/rules.yaml"}`).
The rules map path globs to review packs and to a per-area approval policy;
see [the example](examples/radar-rules.yaml):

```yaml
version: 1
repo_guidance: [CLAUDE.md]      # sent with every review, in the stable prefix
packs: []                       # packs sent with every review, also in the prefix
areas:
  - name: iam
    paths: ["deployer/internal/pulumi/awsiam/**", "**/*kms*"]
    packs: [iam-least-privilege]
    policy: {auto_approve: false}
  - name: migrations
    paths: ["**/migrations/**"]
    packs: [sql-migrations]
    policy: {decline_blocks: true, max_findings: {P2: 0}}
```

A pack is `<id>.md` in the directory passed to `-review-packs`: front matter
with `id` and `version`, then Markdown whose rules are list items that start
with a bold rule ID (`- **GO-ERR-01** ...`); a pack with no rules is rejected.
Rule IDs are unique across packs, and every `repo_guidance` file must be tracked.
Radar sends each reviewer only the packs whose areas the change touches, plus
the nearest-ancestor `AGENTS.md` (`directory_guidance`) of each reviewed file.
The repository guidance and the always-on packs follow the system prompt, so
they form a prefix that is the same for every pull request in the repository
and caches; the selected packs and directory guidance precede the patches.
Guidance is capped at `max_guidance_chars` (default 96000), nearest files
first, and anything cut is marked `truncated`; criteria over half of
`review_chunk_chars` fail safe. Every finding may cite a `rule_id`; a cited ID
no selected pack defines is removed from the finding and listed in
`criteria.unknown_rule_citations` (without criteria, every citation is
removed: reviewers invent IDs when none are given).

The rules and guidance come from the trusted default-branch checkout, and only
files git tracks there count as guidance, so a pull request cannot rewrite the
criteria it is reviewed against. A file withheld as generated selects no packs
and no guidance, but area policy, like deny paths, applies to every changed
path, including withheld files and rename sources.

Every area the change touches applies, and their policies merge to the most
restrictive. Area policy only tightens: `auto_approve: false` (the only allowed
value) routes the change to a human (`pr.area-policy`); `decline_blocks` makes a
decline block even where an author tier would waive the accept claim
(review-findings already blocks on a decline); `max_findings` caps how many
findings of a severity a reviewer may raise and still pass (P0 and P1 always
block, so they may only be capped at 0). Author tiers never relax area policy.
The decision's `criteria` records the rules file's SHA-256, each pack's id,
version and SHA-256 and the areas that selected it, each guidance file's
SHA-256, the areas matched and the merged policy. A policy that names criteria
which were not loaded routes every pull request to a human.

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

### Private author trust tiers (shadow only)

A policy may carry an `author_trust` block that lets a hand-assigned, private
per-author tier relax approval by a bounded amount:

```json
"author_trust": {
  "tiers": [
    {"tier": 2, "min_review_confidence": 7},
    {"tier": 3, "min_review_confidence": 7,
     "rule_limits": [{"rule": "small-change", "max_files": 15, "max_changed_lines": 300}]}
  ]
}
```

A tier may lower `min_review_confidence` by at most one point and, under the
`allow-rules` basis only, raise the limits of named, already-bounded allow rules up
to double and never past the global limits. Under `review-findings` a tier lowers
every reviewer's floor by the same point, but never below `min_review_confidence`
minus one, so it does not stack with `reviewer_min_confidence`. A higher tier must be at least as permissive as a lower one, and an
unconfigured tier inherits the nearest configured tier below it. Tiers never touch
the state gate, deny paths and phrases, the risk threshold, blocking finding
severities, or the zero-risk-signal requirement (so `structural-change` still
routes to a human).

The roster, passed with `-author-trust`, is a JSON object mapping each GitHub
login to an entry with an integer `tier` from 0 to 3 (unlisted means 0). Other
fields in an entry are ignored, so owners can annotate it:
`{"octocat": {"tier": 2, "note": "platform lead"}}`. Only `User` authors get a tier. When the PR body or
any commit carries a coding-agent trailer or session link, the author's tier drops
by one. Under `allow-rules`, below the live confidence minimum the model is told not
to accept, so the tiered gate does not require its accept claim there; everything
else it does. Under `review-findings` a decline still blocks (see DECISIONS.md).

Tiers are **shadow only**: the decision on stdout, and anything Radar posts, is
exactly what it would be without a roster, and never names a tier. The tiered
outcome goes to the file named by `-author-trust-audit` (mode 0600), which the
caller must keep away from anyone who is not a tier owner. A roster that fails to
load is recorded in the audit and leaves the live decision unaffected.

`radar-gh trust-replay -policy P -author-trust R -records R.jsonl` re-decides
stored reviews (`{input, review, commit_messages, outcome}` per line) with and
without tiers from their recorded verdicts, without model calls, and reports the
bad-outcome rate of tier-only approvals next to the live approvals and the
background.

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
