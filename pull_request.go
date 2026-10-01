package radar

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// PullRequestMode controls whether a safe result is reported or may be acted on.
type PullRequestMode string

const (
	PullRequestModeShadow  PullRequestMode = "shadow"
	PullRequestModeApprove PullRequestMode = "approve"
)

// PullRequestAction is the provider-neutral action recommended by the reviewer.
type PullRequestAction string

const (
	PullRequestRouteToHuman    PullRequestAction = "route-to-human"
	PullRequestPolicyCandidate PullRequestAction = "policy-update-candidate"
	PullRequestWouldApprove    PullRequestAction = "would-approve"
	PullRequestApprove         PullRequestAction = "approve"
)

// PullRequestFile is one complete file-level change from a code-review provider.
type PullRequestFile struct {
	Path            string `json:"path"`
	PreviousPath    string `json:"previous_path,omitempty"`
	Status          string `json:"status"`
	Additions       int    `json:"additions"`
	Deletions       int    `json:"deletions"`
	Patch           string `json:"patch,omitempty"`
	ContentComplete bool   `json:"content_complete"`
}

// PullRequestInput is the provider-neutral state required for a review decision.
// Adapters must populate it from current, paginated provider state.
type PullRequestInput struct {
	ID                      string            `json:"id"`
	Repository              string            `json:"repository"`
	Number                  int               `json:"number"`
	Title                   string            `json:"title"`
	Body                    string            `json:"body,omitempty"`
	BaseRef                 string            `json:"base_ref"`
	HeadSHA                 string            `json:"head_sha"`
	Author                  string            `json:"author"`
	AuthorType              string            `json:"author_type,omitempty"`
	Open                    bool              `json:"open"`
	Draft                   bool              `json:"draft"`
	SameRepository          bool              `json:"same_repository"`
	ChecksObserved          bool              `json:"checks_observed"`
	ChecksStable            bool              `json:"checks_stable"`
	ChecksPassing           bool              `json:"checks_passing"`
	CheckFingerprint        string            `json:"check_fingerprint,omitempty"`
	UnresolvedThreads       int               `json:"unresolved_threads"`
	ChangesRequested        bool              `json:"changes_requested"`
	StaleApprovalsDismissed bool              `json:"stale_approvals_dismissed"`
	Files                   []PullRequestFile `json:"files"`
	// Stack lists the open pull requests this one is stacked on, nearest
	// first: Stack[0].HeadRef is BaseRef, and the last entry's BaseRef is the
	// branch the stack ultimately merges into. Empty for an unstacked PR.
	Stack []PullRequestStackEntry `json:"stack,omitempty"`
}

// PullRequestStackEntry is one open pull request below another in a stack.
type PullRequestStackEntry struct {
	Number  int    `json:"number"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
	BaseRef string `json:"base_ref"`
}

// PullRequestApprovalBasis selects what makes a pull request approvable.
type PullRequestApprovalBasis string

const (
	// ApprovalBasisAllowRules requires an allow rule, the global size limits
	// and the risk threshold, then a maximally confident safe review.
	ApprovalBasisAllowRules PullRequestApprovalBasis = "allow-rules"
	// ApprovalBasisReviewFindings rests the verdict on the review agents'
	// findings: no size limits, allow rules or risk gate, and no requirement
	// that the change be one of the safe-signal kinds.
	ApprovalBasisReviewFindings PullRequestApprovalBasis = "review-findings"
)

// PullRequestPathRule names one complete, low-risk class. Every changed file
// must match an include and no exclusion for the rule to apply.
type PullRequestPathRule struct {
	Name            string   `json:"name"`
	Include         []string `json:"include"`
	Exclude         []string `json:"exclude,omitempty"`
	Statuses        []string `json:"statuses"`
	MaxFiles        int      `json:"max_files,omitempty"`
	MaxChangedLines int      `json:"max_changed_lines,omitempty"`
}

// PullRequestPolicy is the versioned, auditable policy for generic PR review.
// Approval requires an explicit allow rule, no deny match, a calibrated risk
// pass, and a maximally confident ACR verdict without risk signals.
type PullRequestPolicy struct {
	Version             int                   `json:"version"`
	Mode                PullRequestMode       `json:"mode"`
	AllowedBaseBranches []string              `json:"allowed_base_branches"`
	AllowRules          []PullRequestPathRule `json:"allow_rules"`
	DenyPaths           []string              `json:"deny_paths,omitempty"`
	DenyPhrases         []string              `json:"deny_phrases,omitempty"`
	MaxFiles            int                   `json:"max_files"`
	MaxChangedLines     int                   `json:"max_changed_lines"`
	MaxRiskPercentile   float64               `json:"max_risk_percentile"`
	CalibrationSample   []float64             `json:"calibration_sample"`
	MinReviewConfidence int                   `json:"min_review_confidence"`
	IgnoredChecks       []string              `json:"ignored_checks,omitempty"`
	AllowSkippedChecks  bool                  `json:"allow_skipped_checks,omitempty"`
	// BlockingFindingSeverities lists the review-finding severities that stop
	// an approval. Empty means P0, P1 and P2, the original criterion. It must
	// always include P0 and P1.
	BlockingFindingSeverities []string `json:"blocking_finding_severities,omitempty"`
	// ApprovalBasis defaults to allow-rules.
	ApprovalBasis PullRequestApprovalBasis `json:"approval_basis,omitempty"`
	// RequiredReviewers is how many review agents must each pass; default 1.
	RequiredReviewers int `json:"required_reviewers,omitempty"`
	// AllowStacked admits a pull request based on another open pull request's
	// head branch when the stack bottoms out on an allowed base branch. A
	// stacked pull request is never approved, only marked would-approve: its
	// verdict covers its own delta, not the changes below it.
	AllowStacked bool `json:"allow_stacked,omitempty"`
	// GeneratedFiles withholds generated files from the review agents.
	GeneratedFiles *PullRequestGeneratedFiles `json:"generated_files,omitempty"`
	// Lockfiles reviews lockfiles instead of withholding them.
	Lockfiles *PullRequestLockfiles `json:"lockfiles,omitempty"`
	// ReviewChunkChars caps the patch text sent in one review request; larger
	// diffs are reviewed in several. Zero means defaultReviewChunkChars.
	ReviewChunkChars int `json:"review_chunk_chars,omitempty"`
	// ReviewerMinConfidence overrides min_review_confidence for a reviewer,
	// keyed by its provenance (e.g. "bedrock/us.anthropic.claude-sonnet-5").
	// Review-findings only, so a reviewer passing at its floor has still
	// raised no blocking finding and no defect signal. A floor may sit at
	// most one point below min_review_confidence.
	ReviewerMinConfidence map[string]int `json:"reviewer_min_confidence,omitempty"`
}

// maxConfidenceRelief is how far a reviewer's floor may sit below the
// policy's min_review_confidence.
const maxConfidenceRelief = 1

func (p PullRequestPolicy) minConfidenceFor(reviewer string) int {
	if floor, ok := p.ReviewerMinConfidence[reviewer]; ok {
		return floor
	}
	return p.MinReviewConfidence
}

// PullRequestReview is a strict, machine-readable decision bound to one head.
type PullRequestReview struct {
	SchemaVersion  int               `json:"schema_version"`
	RequestID      string            `json:"request_id"`
	HeadSHA        string            `json:"head_sha"`
	PolicyVersion  int               `json:"policy_version"`
	Mode           PullRequestMode   `json:"mode"`
	Action         PullRequestAction `json:"action"`
	Eligible       bool              `json:"eligible"`
	MatchedRule    string            `json:"matched_rule,omitempty"`
	RawRiskScore   float64           `json:"raw_risk_score"`
	RiskPercentile float64           `json:"risk_percentile"`
	// Reviewer names the ACR provider and model that produced Agent, e.g.
	// "openai/gpt-4o-mini" or "fireworks/accounts/fireworks/models/glm-5p3".
	// With several reviewers it joins their names with "+", and Agent is the
	// conservative merge of their verdicts; Reviews holds each one.
	Reviewer      string                       `json:"reviewer,omitempty"`
	Agent         ACRResult                    `json:"agent"`
	Reviews       []PullRequestReviewerVerdict `json:"reviews,omitempty"`
	ApprovalBasis PullRequestApprovalBasis     `json:"approval_basis"`
	Stack         []PullRequestStackEntry      `json:"stack,omitempty"`
	// Withheld are the generated files the agents saw only by name.
	Withheld []GeneratedFile `json:"withheld,omitempty"`
	// Lockfiles summarises each changed lockfile the agents reviewed.
	Lockfiles []LockfileSummary `json:"lockfiles,omitempty"`
	// GeneratedUnlisted are reviewed files whose generated-code header the
	// change itself adds, outside every configured generated path.
	GeneratedUnlisted []string      `json:"generated_unlisted,omitempty"`
	Stages            []StageResult `json:"stages"`
}

// PullRequestReviewerVerdict is one review agent's verdict and whether it met
// the policy's bar.
type PullRequestReviewerVerdict struct {
	Reviewer string    `json:"reviewer"`
	Passed   bool      `json:"passed"`
	Reason   string    `json:"reason"`
	Agent    ACRResult `json:"agent"`
}

// PullRequestReviewer applies a policy with pluggable Radar scoring and review.
type PullRequestReviewer struct {
	policy     PullRequestPolicy
	scorer     RiskScorer
	calibrator *Calibrator
	agents     []ReviewAgent
	generated  generatedMatcher
}

// NewPullRequestReviewer validates policy and constructs a generic PR
// reviewer. With several agents, each must pass on its own.
func NewPullRequestReviewer(policy PullRequestPolicy, scorer RiskScorer, agents ...ReviewAgent) (*PullRequestReviewer, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if scorer == nil {
		return nil, fmt.Errorf("radar: pull request scorer is required")
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("radar: pull request review agent is required")
	}
	for _, agent := range agents {
		if agent == nil {
			return nil, fmt.Errorf("radar: pull request review agent is required")
		}
	}
	if len(agents) < policy.requiredReviewers() {
		return nil, fmt.Errorf("radar: policy requires %d reviewers, got %d", policy.requiredReviewers(), len(agents))
	}
	r := &PullRequestReviewer{
		policy:     policy,
		scorer:     scorer,
		calibrator: NewCalibrator(policy.CalibrationSample),
		agents:     agents,
	}
	if policy.GeneratedFiles != nil {
		r.generated = newGeneratedMatcher(*policy.GeneratedFiles, nil)
	}
	return r, nil
}

// UseGitattributes adds the linguist-generated paths of a trusted
// .gitattributes file to the policy's generated paths.
func (r *PullRequestReviewer) UseGitattributes(data []byte) error {
	if r.policy.GeneratedFiles == nil {
		return fmt.Errorf("radar: policy has no generated_files block")
	}
	paths, err := ParseGitattributesGenerated(data)
	if err != nil {
		return err
	}
	r.generated = newGeneratedMatcher(*r.policy.GeneratedFiles, paths)
	return nil
}

func (p PullRequestPolicy) basis() PullRequestApprovalBasis {
	if p.ApprovalBasis == "" {
		return ApprovalBasisAllowRules
	}
	return p.ApprovalBasis
}

func (p PullRequestPolicy) requiredReviewers() int {
	if p.RequiredReviewers <= 0 {
		return 1
	}
	return p.RequiredReviewers
}

func (p PullRequestPolicy) reviewChunkChars() int {
	if p.ReviewChunkChars <= 0 {
		return defaultReviewChunkChars
	}
	return p.ReviewChunkChars
}

// Validate rejects policies that could silently broaden automation.
func (p PullRequestPolicy) Validate() error {
	if p.Version <= 0 {
		return fmt.Errorf("radar: pull request policy version must be positive")
	}
	if p.Mode != PullRequestModeShadow && p.Mode != PullRequestModeApprove {
		return fmt.Errorf("radar: pull request policy mode must be shadow or approve")
	}
	if len(p.AllowedBaseBranches) == 0 {
		return fmt.Errorf("radar: at least one allowed base branch is required")
	}
	switch p.basis() {
	case ApprovalBasisAllowRules:
		if len(p.AllowRules) == 0 {
			return fmt.Errorf("radar: at least one allow rule is required")
		}
		if p.MaxFiles <= 0 || p.MaxChangedLines <= 0 {
			return fmt.Errorf("radar: positive global file and line limits are required")
		}
	case ApprovalBasisReviewFindings:
		// Limits that would be ignored must not look as if they apply.
		if len(p.AllowRules) > 0 || p.MaxFiles != 0 || p.MaxChangedLines != 0 {
			return fmt.Errorf("radar: approval basis review-findings takes no allow rules or global file and line limits")
		}
	default:
		return fmt.Errorf("radar: approval basis must be allow-rules or review-findings")
	}
	if len(p.ReviewerMinConfidence) > 0 && p.basis() != ApprovalBasisReviewFindings {
		return fmt.Errorf("radar: reviewer_min_confidence requires approval basis review-findings")
	}
	for reviewer, floor := range p.ReviewerMinConfidence {
		if strings.TrimSpace(reviewer) == "" || floor < p.MinReviewConfidence-maxConfidenceRelief || floor > ACRMaxConfidence {
			return fmt.Errorf("radar: reviewer_min_confidence for %q must be between %d and %d", reviewer, p.MinReviewConfidence-maxConfidenceRelief, ACRMaxConfidence)
		}
	}
	if p.RequiredReviewers < 0 || p.ReviewChunkChars < 0 {
		return fmt.Errorf("radar: required reviewers and review chunk size cannot be negative")
	}
	if p.GeneratedFiles != nil {
		if err := p.GeneratedFiles.validate(); err != nil {
			return err
		}
	}
	if p.MaxRiskPercentile < 0 || p.MaxRiskPercentile > 100 {
		return fmt.Errorf("radar: max risk percentile must be between 0 and 100")
	}
	if len(p.CalibrationSample) == 0 {
		return fmt.Errorf("radar: an explicit calibration sample is required")
	}
	for _, score := range p.CalibrationSample {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return fmt.Errorf("radar: calibration scores must be finite")
		}
	}
	if p.MinReviewConfidence < ACRMinConfidence || p.MinReviewConfidence > ACRMaxConfidence {
		return fmt.Errorf("radar: review confidence must be between %d and %d", ACRMinConfidence, ACRMaxConfidence)
	}
	if len(p.BlockingFindingSeverities) > 0 {
		seen := map[string]bool{}
		for _, s := range p.BlockingFindingSeverities {
			if !containsFold([]string{"P0", "P1", "P2", "P3"}, s) {
				return fmt.Errorf("radar: unknown blocking finding severity %q", s)
			}
			seen[strings.ToUpper(s)] = true
		}
		if !seen["P0"] || !seen["P1"] {
			return fmt.Errorf("radar: blocking finding severities must include P0 and P1")
		}
	}
	seen := map[string]bool{}
	for _, r := range p.AllowRules {
		if strings.TrimSpace(r.Name) == "" || seen[r.Name] {
			return fmt.Errorf("radar: allow rule names must be non-empty and unique")
		}
		seen[r.Name] = true
		if len(r.Include) == 0 || len(r.Statuses) == 0 {
			return fmt.Errorf("radar: allow rule %q requires include patterns and statuses", r.Name)
		}
		if r.MaxFiles < 0 || r.MaxChangedLines < 0 {
			return fmt.Errorf("radar: allow rule %q limits cannot be negative", r.Name)
		}
		for _, status := range r.Statuses {
			if !containsFold([]string{"added", "modified", "removed", "renamed", "copied", "changed", "unchanged"}, status) {
				return fmt.Errorf("radar: allow rule %q has unknown status %q", r.Name, status)
			}
		}
		for _, pattern := range append(append([]string{}, r.Include...), r.Exclude...) {
			if _, err := compilePathGlob(pattern); err != nil {
				return fmt.Errorf("radar: allow rule %q: %w", r.Name, err)
			}
		}
	}
	for _, pattern := range p.DenyPaths {
		if _, err := compilePathGlob(pattern); err != nil {
			return err
		}
	}
	return nil
}

// Review evaluates current PR state. It never performs provider mutations.
func (r *PullRequestReviewer) Review(in PullRequestInput) PullRequestReview {
	out := PullRequestReview{
		SchemaVersion:  1,
		RequestID:      in.ID,
		HeadSHA:        in.HeadSHA,
		PolicyVersion:  r.policy.Version,
		Mode:           r.policy.Mode,
		Action:         PullRequestRouteToHuman,
		RiskPercentile: -1,
		Reviewer:       r.describeReviewers(),
		ApprovalBasis:  r.policy.basis(),
		Stack:          in.Stack,
	}
	add := func(name string, passed bool, reason string) bool {
		out.Stages = append(out.Stages, StageResult{Name: name, Passed: passed, Reason: reason})
		return passed
	}

	if ok, reason := reviewStateGate(r.policy, in); !add("pr.state", ok, reason) {
		return out
	}

	diff, complete, reason := pullRequestDiff(in)
	if !add("pr.complete-diff", complete, reason) {
		return out
	}

	denied, denyReason := pullRequestDenied(r.policy, in)
	add("pr.deny-policy", !denied, denyReason)

	lockfiles := r.lockfileSummaries(in)
	out.Lockfiles = lockfiles
	if r.policy.Lockfiles != nil && len(lockfiles) > 0 {
		backstop, reason := lockfileBackstop(*r.policy.Lockfiles, in.Files, lockfiles)
		add("pr.lockfile-policy", !backstop, reason)
		denied = denied || backstop
	}

	allowlisted := true
	if r.policy.basis() == ApprovalBasisAllowRules {
		rule, allowReason := matchPullRequestRule(r.policy, in)
		allowlisted = rule != nil
		add("pr.allow-rule", allowlisted, allowReason)
		if rule != nil {
			out.MatchedRule = rule.Name
		}
	}

	out.RawRiskScore = r.scorer.Score(diff)
	if math.IsNaN(out.RawRiskScore) || math.IsInf(out.RawRiskScore, 0) {
		add("pr.risk-threshold", false, "risk scorer returned a non-finite value")
		return out
	}
	out.RiskPercentile = r.calibrator.Percentile(out.RawRiskScore)
	riskPassed := true
	if r.policy.basis() == ApprovalBasisAllowRules {
		riskPassed = out.RiskPercentile <= r.policy.MaxRiskPercentile
		add("pr.risk-threshold", riskPassed,
			fmt.Sprintf("risk percentile %.1f vs threshold P%.1f", out.RiskPercentile, r.policy.MaxRiskPercentile))
	} else {
		// The heuristic score grows with line count, so gating on it would
		// reintroduce the size limit this basis drops.
		add("pr.risk-score", true, fmt.Sprintf("risk percentile %.1f recorded, not gating under review-findings", out.RiskPercentile))
	}

	reviewDiff := r.reviewableDiff(diff, in, lockfiles)
	out.Withheld = reviewDiff.Withheld
	for _, f := range in.Files {
		if _, unlisted := r.generated.classify(f); unlisted {
			out.GeneratedUnlisted = append(out.GeneratedUnlisted, f.Path)
		}
	}
	agentPassed := false
	if len(reviewDiff.Changes) == 0 {
		add("pr.review-agent", false, fmt.Sprintf("all %d changed files are generated; nothing for the review agents to assess", len(out.Withheld)))
	} else {
		agentPassed = r.runReviewers(&out, reviewDiff, add)
	}

	out.Eligible = !denied && allowlisted && riskPassed
	switch {
	case out.Eligible && agentPassed && len(in.Stack) > 0:
		add("pr.stack", true, fmt.Sprintf("stacked on #%d; the verdict covers this pull request's own changes only", in.Stack[0].Number))
		out.Action = PullRequestWouldApprove
	case out.Eligible && agentPassed && r.policy.Mode == PullRequestModeApprove:
		out.Action = PullRequestApprove
	case out.Eligible && agentPassed:
		out.Action = PullRequestWouldApprove
	case !denied && !out.Eligible && agentPassed:
		out.Action = PullRequestPolicyCandidate
	}
	return out
}

func (r *PullRequestReviewer) lockfileSummaries(in PullRequestInput) []LockfileSummary {
	if r.policy.Lockfiles == nil {
		return nil
	}
	var out []LockfileSummary
	for _, f := range in.Files {
		if kind, ok := lockfileKindOf(f.Path); ok {
			out = append(out, summarizeLockfile(f.Path, kind, f.Patch))
		}
	}
	return out
}

// reviewableDiff withholds generated files from what the agents see. With
// lockfile review on, lockfiles are kept: one whose +/- lines repeat an
// earlier lockfile's is shown as a pointer to it, and one too large for a
// single request is split by hunk so every line is still reviewed.
func (r *PullRequestReviewer) reviewableDiff(diff Diff, in PullRequestInput, lockfiles []LockfileSummary) Diff {
	out := diff
	out.Changes = nil
	out.Lockfiles = lockfiles
	summaries := map[string]LockfileSummary{}
	for _, s := range lockfiles {
		summaries[s.Path] = s
	}
	firstWith := map[string]string{}
	budget := r.policy.reviewChunkChars() - chunkOverheadChars - 512
	for i, f := range in.Files {
		change := diff.Changes[i]
		if summary, isLock := summaries[f.Path]; isLock {
			key := changeLinesKey(change, summary)
			if first, seen := firstWith[key]; seen {
				change.Content = "(identical +/- lines and summary to " + first + "; context omitted)"
				out.Changes = append(out.Changes, change)
				continue
			}
			firstWith[key] = f.Path
			pieces := splitLockfilePatch(change.Content, budget)
			for j, piece := range pieces {
				part := change
				part.Content = piece
				if len(pieces) > 1 {
					part.Content = fmt.Sprintf("(lockfile patch piece %d of %d)\n%s", j+1, len(pieces), piece)
				}
				out.Changes = append(out.Changes, part)
			}
			continue
		}
		if reason, _ := r.generated.classify(f); reason != "" {
			out.Withheld = append(out.Withheld, GeneratedFile{
				Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions, Reason: reason,
			})
			continue
		}
		out.Changes = append(out.Changes, change)
	}
	if len(out.Withheld) > 0 && r.policy.GeneratedFiles != nil && r.policy.GeneratedFiles.Note != "" {
		out.ReviewNotes = append(out.ReviewNotes, r.policy.GeneratedFiles.Note)
	}
	return out
}

// runReviewers runs every agent concurrently and records each verdict. It
// passes only when every agent passes.
func (r *PullRequestReviewer) runReviewers(out *PullRequestReview, diff Diff, add func(string, bool, string) bool) bool {
	results := make([]ACRResult, len(r.agents))
	var wg sync.WaitGroup
	for i, agent := range r.agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = reviewInChunks(agent, diff, r.policy.reviewChunkChars())
		}()
	}
	wg.Wait()

	passed := 0
	for i, res := range results {
		ok, reason := r.reviewerPasses(describeReviewAgent(r.agents[i]), res, diff)
		if ok {
			passed++
		}
		out.Reviews = append(out.Reviews, PullRequestReviewerVerdict{
			Reviewer: describeReviewAgent(r.agents[i]), Passed: ok, Reason: reason, Agent: res,
		})
	}
	if len(results) == 1 {
		out.Agent = results[0]
		reason := out.Reviews[0].Reason
		out.Reviews = nil
		return add("pr.review-agent", passed == 1, reason)
	}
	out.Agent = mergeVerdicts(results, func(i int) string { return describeReviewAgent(r.agents[i]) })
	for _, review := range out.Reviews {
		add("pr.review-agent", review.Passed, review.Reviewer+": "+review.Reason)
	}
	return add("pr.review-consensus", passed == len(results),
		fmt.Sprintf("%d of %d reviewers passed; every reviewer must", passed, len(results)))
}

// defectSignals are the risk signals that describe a likely problem rather
// than the change's size or shape; only they block under review-findings.
var defectSignals = map[ChangeSignal]bool{
	SignalBugOrLogicError: true,
	SignalPerformanceRisk: true,
	SignalSecretsExposure: true,
	SignalSQLInjection:    true,
	SignalAuthBypass:      true,
}

func (r *PullRequestReviewer) reviewerPasses(reviewer string, res ACRResult, diff Diff) (bool, string) {
	blocking := r.policy.blockingSeverities()
	hasBlocking := hasBlockingFinding(res.Findings, blocking)
	covered := reviewCoversDiff(res, diff) && !res.Truncated
	minConfidence := r.policy.MinReviewConfidence
	if r.policy.basis() == ApprovalBasisReviewFindings {
		minConfidence = r.policy.minConfidenceFor(reviewer)
	}
	confident := res.Confidence >= minConfidence
	var passed bool
	var detail string
	switch r.policy.basis() {
	case ApprovalBasisReviewFindings:
		var defects []string
		for _, s := range res.RiskSignals {
			if defectSignals[s] {
				defects = append(defects, string(s))
			}
		}
		// A decline must be explained by a recorded signal or finding; one
		// with neither is a concern the reviewer left unstated.
		explained := res.ModelAccept || res.Accept || len(res.RiskSignals) > 0 || len(res.Findings) > 0
		passed = confident && covered && !hasBlocking && len(defects) == 0 && explained
		detail = fmt.Sprintf("confidence=%d/%d defect-signals=%v declined-unexplained=%t", res.Confidence, minConfidence, defects, !explained)
	default:
		claimed := res.Accept || res.ModelAccept
		passed = claimed && confident && len(res.RiskSignals) == 0 && len(res.SafeSignals) > 0 && covered && !hasBlocking
		detail = fmt.Sprintf("accept=%t confidence=%d/%d risk-signals=%d", claimed, res.Confidence, r.policy.MinReviewConfidence, len(res.RiskSignals))
	}
	return passed, fmt.Sprintf("%s reviewed-files=%d/%d blocking-findings(%s)=%t: %s",
		detail, len(res.ReviewedFiles), len(diff.Changes), strings.Join(blocking, ","), hasBlocking, res.Summary)
}

func (r *PullRequestReviewer) describeReviewers() string {
	names := make([]string, 0, len(r.agents))
	for _, agent := range r.agents {
		names = append(names, describeReviewAgent(agent))
	}
	return strings.Join(names, "+")
}

// reviewCoversDiff requires the agent to name exactly the files it was
// shown; a file split into several pieces counts once.
func reviewCoversDiff(result ACRResult, diff Diff) bool {
	want := map[string]bool{}
	for _, change := range diff.Changes {
		want[change.File] = true
	}
	got := map[string]bool{}
	for _, file := range result.ReviewedFiles {
		if !want[file] {
			return false
		}
		got[file] = true
	}
	return len(got) == len(want)
}

// blockingSeverities returns the finding severities that block approval,
// defaulting to P0-P2 for policies that predate the setting.
func (p PullRequestPolicy) blockingSeverities() []string {
	if len(p.BlockingFindingSeverities) == 0 {
		return []string{"P0", "P1", "P2"}
	}
	out := make([]string, 0, len(p.BlockingFindingSeverities))
	for _, s := range p.BlockingFindingSeverities {
		out = append(out, strings.ToUpper(s))
	}
	return out
}

func hasBlockingFinding(findings []ReviewFinding, blocking []string) bool {
	for _, finding := range findings {
		if containsFold(blocking, finding.Severity) {
			return true
		}
	}
	return false
}

func reviewStateGate(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	switch {
	case strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.HeadSHA) == "":
		return false, "request id and head SHA are required"
	case !in.Open:
		return false, "pull request is not open"
	case in.Draft:
		return false, "pull request is a draft"
	case !in.SameRepository:
		return false, "cross-repository pull requests are not eligible"
	case !matchesAnyPath(in.BaseRef, policy.AllowedBaseBranches) && len(in.Stack) == 0:
		return false, "base branch is not allowed"
	case len(in.Stack) > 0 && !policy.AllowStacked:
		return false, "stacked pull requests are not enabled by policy"
	case !in.ChecksObserved:
		return false, "no checks were observed"
	case strings.TrimSpace(in.CheckFingerprint) == "":
		return false, "check fingerprint is missing"
	case !in.ChecksStable:
		return false, "check surface is not stable"
	case !in.ChecksPassing:
		return false, "checks are not passing"
	case in.UnresolvedThreads > 0:
		return false, fmt.Sprintf("%d unresolved review thread(s)", in.UnresolvedThreads)
	case in.ChangesRequested:
		return false, "changes have been requested"
	case policy.Mode == PullRequestModeApprove && !in.StaleApprovalsDismissed:
		return false, "approval mode requires stale approvals to be dismissed"
	}
	if len(in.Stack) > 0 {
		if err := validateStack(policy, in); err != nil {
			return false, err.Error()
		}
		root := in.Stack[len(in.Stack)-1].BaseRef
		return true, fmt.Sprintf("pull request state is eligible; stacked %d deep on %s", len(in.Stack), root)
	}
	return true, "pull request state is eligible"
}

// validateStack checks the stack is a chain of distinct open pull requests
// from this one's base down to an allowed base branch.
func validateStack(policy PullRequestPolicy, in PullRequestInput) error {
	want := in.BaseRef
	seen := map[int]bool{in.Number: true}
	for _, entry := range in.Stack {
		switch {
		case entry.Number <= 0 || seen[entry.Number]:
			return fmt.Errorf("stack repeats or omits a pull request number")
		case entry.HeadRef != want:
			return fmt.Errorf("stack is broken: expected #%d to have head %s, found %s", entry.Number, want, entry.HeadRef)
		case strings.TrimSpace(entry.HeadSHA) == "":
			return fmt.Errorf("stack entry #%d has no head SHA", entry.Number)
		}
		seen[entry.Number] = true
		want = entry.BaseRef
	}
	if !matchesAnyPath(want, policy.AllowedBaseBranches) {
		return fmt.Errorf("stack bottoms out on %s, which is not an allowed base branch", want)
	}
	return nil
}

func pullRequestDiff(in PullRequestInput) (Diff, bool, string) {
	if len(in.Files) == 0 {
		return Diff{}, false, "pull request has no changed files"
	}
	changes := make([]Change, 0, len(in.Files))
	seen := map[string]bool{}
	for _, f := range in.Files {
		if err := validateRepositoryPath(f.Path); err != nil {
			return Diff{}, false, err.Error()
		}
		if f.PreviousPath != "" {
			if err := validateRepositoryPath(f.PreviousPath); err != nil {
				return Diff{}, false, err.Error()
			}
		}
		if seen[f.Path] {
			return Diff{}, false, "provider returned duplicate changed path " + f.Path
		}
		seen[f.Path] = true
		if f.Additions < 0 || f.Deletions < 0 {
			return Diff{}, false, "provider returned negative line counts for " + f.Path
		}
		if !f.ContentComplete {
			return Diff{}, false, "provider did not return a complete patch for " + f.Path
		}
		changes = append(changes, Change{
			File: f.Path, PreviousFile: f.PreviousPath, Type: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Content: f.Patch,
		})
	}
	return Diff{
		ID: in.ID, Org: in.Repository, Source: SourceHuman,
		Author: Author{Name: in.Author}, CI: CIPassing, State: DiffPublished,
		Changes: changes,
	}, true, "complete provider diff"
}

func pullRequestDenied(policy PullRequestPolicy, in PullRequestInput) (bool, string) {
	for _, f := range in.Files {
		for _, candidate := range []string{f.Path, f.PreviousPath} {
			if candidate != "" && matchesAnyPath(candidate, policy.DenyPaths) {
				return true, "path is explicitly denied: " + candidate
			}
		}
	}
	text := strings.ToLower(in.Title + "\n" + in.Body)
	for _, f := range in.Files {
		text += "\n" + strings.ToLower(f.Patch)
	}
	for _, phrase := range policy.DenyPhrases {
		if phrase != "" && strings.Contains(text, strings.ToLower(phrase)) {
			return true, "content contains denied phrase: " + phrase
		}
	}
	return false, "no explicit deny rule matched"
}

func matchPullRequestRule(policy PullRequestPolicy, in PullRequestInput) (*PullRequestPathRule, string) {
	lines := 0
	for _, f := range in.Files {
		lines += f.Additions + f.Deletions
	}
	if len(in.Files) > policy.MaxFiles {
		return nil, fmt.Sprintf("%d files exceeds global limit %d", len(in.Files), policy.MaxFiles)
	}
	if lines > policy.MaxChangedLines {
		return nil, fmt.Sprintf("%d changed lines exceeds global limit %d", lines, policy.MaxChangedLines)
	}

	for i := range policy.AllowRules {
		rule := &policy.AllowRules[i]
		if rule.MaxFiles > 0 && len(in.Files) > rule.MaxFiles {
			continue
		}
		if rule.MaxChangedLines > 0 && lines > rule.MaxChangedLines {
			continue
		}
		ok := true
		for _, f := range in.Files {
			if !matchesAnyPath(f.Path, rule.Include) || matchesAnyPath(f.Path, rule.Exclude) ||
				!containsFold(rule.Statuses, f.Status) {
				ok = false
				break
			}
			if f.PreviousPath != "" && (!matchesAnyPath(f.PreviousPath, rule.Include) || matchesAnyPath(f.PreviousPath, rule.Exclude)) {
				ok = false
				break
			}
		}
		if ok {
			return rule, "matched allow rule: " + rule.Name
		}
	}
	return nil, "no allow rule covers the complete change"
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func validateRepositoryPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("invalid repository path %q", value)
	}
	return nil
}

// MatchesPathGlob reports whether value matches any of Radar's path globs.
func MatchesPathGlob(value string, patterns []string) bool { return matchesAnyPath(value, patterns) }

func matchesAnyPath(value string, patterns []string) bool {
	for _, pattern := range patterns {
		re, err := compilePathGlob(pattern)
		if err == nil && re.MatchString(value) {
			return true
		}
	}
	return false
}

func compilePathGlob(glob string) (*regexp.Regexp, error) {
	if glob == "" || strings.HasPrefix(glob, "/") || strings.Contains(glob, "\\") || strings.Contains(glob, "..") {
		return nil, fmt.Errorf("invalid path pattern %q", glob)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				if i+2 < len(glob) && glob[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// SortedIgnoredChecks returns the configured check exclusions in stable order.
func (p PullRequestPolicy) SortedIgnoredChecks() []string {
	checks := append([]string(nil), p.IgnoredChecks...)
	sort.Strings(checks)
	return checks
}
