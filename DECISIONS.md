# Decisions

Policy decisions behind Radar's review-findings basis, newest first. Each
names who decided and the evidence; the replay data lives with the Linear
issue.

## A reviewer's explicit decline blocks approval (INF-1220, 2026-10-01)

Under `approval_basis: review-findings`, a reviewer passes only if it accepts
(`accept: true`). A decline blocks approval whatever its findings, signals or
confidence.

Why: review-findings deliberately lets size and shape signals
(`structural-change`, `high-review-effort`) through, and reviewers often say
"a human should look" only by declining with one of those signals. In the
45-PR replay with GLM-5.3 and Codex (`openai.gpt-5.6-terra`), Codex wrote
"requires human review" in 19 summaries, and 7 of the pair's 23 approvals
were PRs Codex itself had declined. With declines blocking, the pair approves
16. Decided by TJ (option 2 of three: as-is, declines block, or keep
GLM-5.3 + Sonnet 5).

The reviewer pair became GLM-5.3 on Fireworks plus Codex on Bedrock Mantle,
replacing Claude Sonnet 5 (INF-1223 found Sonnet's P0/P1 findings a subset of
GLM's, and Codex the most precise on P0/P1). Codex reports confidence 8-10,
so it needs no `reviewer_min_confidence` entry.

## Per-reviewer confidence floor (INF-1220, 2026-10-01)

`reviewer_min_confidence` lowers `min_review_confidence` for one reviewer by
at most one point, under review-findings only. It was added for Sonnet 5,
which failed 14 of 45 replayed PRs on confidence 6-7 alone. Removing the floor
entirely was rejected: the 4 extra approvals were security- or
compliance-adjacent changes the reviewers had judged risky without naming a
defect.

## Decide from review findings, not size (INF-1220, 2026-09-30)

`approval_basis: review-findings` drops allow rules, global file and line
limits, and the risk threshold (the heuristic score grows with line count, so
gating on it is a size cap in disguise). The state gate, deny paths and
phrases, lockfile backstops and generated-file withholding still apply.
Decided by TJ: approval should rest on what the reviewers find, not on size.
