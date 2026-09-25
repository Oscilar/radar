package radar

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxAuthorTrustTier is the highest tier a trust roster may assign.
const MaxAuthorTrustTier = 3

// PullRequestAuthorTrust bounds how far a private per-author trust tier may
// relax approval. Tier 0, and every author the roster does not list, get the
// policy unchanged. Tiers never touch the state gate, deny rules, risk
// threshold, blocking finding severities, or the zero-risk-signal requirement.
type PullRequestAuthorTrust struct {
	Tiers []PullRequestTrustTier `json:"tiers"`
}

// PullRequestTrustTier is the complete adjustment for one tier. A tier that is
// not configured inherits the nearest configured tier below it.
type PullRequestTrustTier struct {
	Tier int `json:"tier"`
	// MinReviewConfidence replaces the policy minimum. Zero keeps it. It may be
	// at most one point below the policy minimum.
	MinReviewConfidence int `json:"min_review_confidence,omitempty"`
	// RuleLimits raise the size limits of named, already-bounded allow rules.
	RuleLimits []PullRequestRuleLimit `json:"rule_limits,omitempty"`
}

// PullRequestRuleLimit raises one allow rule's file and line limits.
type PullRequestRuleLimit struct {
	Rule            string `json:"rule"`
	MaxFiles        int    `json:"max_files"`
	MaxChangedLines int    `json:"max_changed_lines"`
}

// AuthorTrust is one pull request author's private trust state.
type AuthorTrust struct {
	// Tier is the roster tier, 0 for authors the roster does not list.
	Tier int
	// AgentAuthored is set when a coding agent wrote the change under the
	// author's account, so the author's tier does not vouch for it in full.
	AgentAuthored bool
}

// EffectiveTier is the tier that applies to this change.
func (t AuthorTrust) EffectiveTier() int {
	if t.AgentAuthored && t.Tier > 0 {
		return t.Tier - 1
	}
	return t.Tier
}

// AuthorTrustAudit is the private record of a tiered shadow evaluation. It
// names the author's tier, so it must only reach the tier owners; the public
// PullRequestReview never carries any of it.
type AuthorTrustAudit struct {
	SchemaVersion       int               `json:"schema_version"`
	RequestID           string            `json:"request_id"`
	HeadSHA             string            `json:"head_sha"`
	PolicyVersion       int               `json:"policy_version"`
	Reviewer            string            `json:"reviewer,omitempty"`
	Author              string            `json:"author"`
	Tier                int               `json:"tier"`
	AgentAuthored       bool              `json:"agent_authored"`
	EffectiveTier       int               `json:"effective_tier"`
	MinReviewConfidence int               `json:"min_review_confidence"`
	LiveAction          PullRequestAction `json:"live_action"`
	TierAction          PullRequestAction `json:"tier_action"`
	// Adjusted reports whether the tier changed the decision.
	Adjusted    bool          `json:"adjusted"`
	MatchedRule string        `json:"matched_rule,omitempty"`
	Stages      []StageResult `json:"stages,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// AuthorTrustRoster maps lower-cased GitHub logins to trust tiers.
type AuthorTrustRoster map[string]int

// Tier returns the login's tier, 0 when unlisted.
func (r AuthorTrustRoster) Tier(login string) int {
	return r[strings.ToLower(login)]
}

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// ParseAuthorTrustRoster reads a flat YAML mapping of GitHub login to tier:
//
//	# comment
//	octocat: 2
//
// Anything else is rejected. Errors cite line numbers only, because the roster
// is private and errors reach public logs.
func ParseAuthorTrustRoster(data []byte) (AuthorTrustRoster, error) {
	roster := AuthorTrustRoster{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		login, value, ok := strings.Cut(line, ":")
		login = strings.TrimSpace(login)
		value = strings.TrimSpace(value)
		if !ok || !githubLogin.MatchString(login) {
			return nil, fmt.Errorf("radar: trust roster line %d is not \"login: tier\"", n)
		}
		tier, err := strconv.Atoi(value)
		if err != nil || tier < 0 || tier > MaxAuthorTrustTier {
			return nil, fmt.Errorf("radar: trust roster line %d: tier must be 0-%d", n, MaxAuthorTrustTier)
		}
		key := strings.ToLower(login)
		if _, dup := roster[key]; dup {
			return nil, fmt.Errorf("radar: trust roster line %d repeats a login", n)
		}
		roster[key] = tier
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("radar: reading trust roster: %w", err)
	}
	return roster, nil
}

var agentAuthorshipMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*co-authored-by:.*(claude|codex|openai|anthropic|cursor|copilot|devin|aider|gemini)`),
	regexp.MustCompile(`(?im)^\s*(claude|codex)-session:`),
	regexp.MustCompile(`(?i)claude\.ai/code/session_|chatgpt\.com/codex|generated with \[?claude code`),
}

// AgentAuthored reports whether a PR body or any commit message carries a
// coding-agent trailer or session link. It errs towards true: a false
// positive only lowers the author's tier.
func AgentAuthored(body string, commitMessages []string) bool {
	for _, text := range append([]string{body}, commitMessages...) {
		for _, marker := range agentAuthorshipMarkers {
			if marker.MatchString(text) {
				return true
			}
		}
	}
	return false
}

func (p PullRequestPolicy) validateAuthorTrust() error {
	if p.AuthorTrust == nil {
		return nil
	}
	rules := map[string]PullRequestPathRule{}
	for _, r := range p.AllowRules {
		rules[r.Name] = r
	}
	seen := map[int]bool{}
	for _, t := range p.AuthorTrust.Tiers {
		if t.Tier < 1 || t.Tier > MaxAuthorTrustTier || seen[t.Tier] {
			return fmt.Errorf("radar: author trust tiers must be unique and between 1 and %d", MaxAuthorTrustTier)
		}
		seen[t.Tier] = true
		if c := t.MinReviewConfidence; c != 0 && (c < p.MinReviewConfidence-1 || c > p.MinReviewConfidence) {
			return fmt.Errorf("radar: author trust tier %d may lower review confidence by at most one point", t.Tier)
		}
		limited := map[string]bool{}
		for _, l := range t.RuleLimits {
			rule, ok := rules[l.Rule]
			switch {
			case !ok || limited[l.Rule]:
				return fmt.Errorf("radar: author trust tier %d must name each existing allow rule at most once", t.Tier)
			case rule.MaxFiles <= 0 || rule.MaxChangedLines <= 0:
				return fmt.Errorf("radar: author trust tier %d may only raise limits of a bounded allow rule", t.Tier)
			case l.MaxFiles < rule.MaxFiles || l.MaxChangedLines < rule.MaxChangedLines:
				return fmt.Errorf("radar: author trust tier %d cannot tighten allow rule %q", t.Tier, l.Rule)
			case l.MaxFiles > 2*rule.MaxFiles || l.MaxChangedLines > 2*rule.MaxChangedLines:
				return fmt.Errorf("radar: author trust tier %d may at most double allow rule %q limits", t.Tier, l.Rule)
			case l.MaxFiles > p.MaxFiles || l.MaxChangedLines > p.MaxChangedLines:
				return fmt.Errorf("radar: author trust tier %d cannot exceed the global limits", t.Tier)
			}
			limited[l.Rule] = true
		}
	}
	// A higher tier must be at least as permissive as every lower one, so the
	// agent-authorship step down never relaxes anything.
	for tier := 2; tier <= MaxAuthorTrustTier; tier++ {
		lower, higher := p.withTrustTier(tier-1), p.withTrustTier(tier)
		if higher.MinReviewConfidence > lower.MinReviewConfidence {
			return fmt.Errorf("radar: author trust tier %d is stricter than tier %d", tier, tier-1)
		}
		for i := range higher.AllowRules {
			h, l := higher.AllowRules[i], lower.AllowRules[i]
			if h.MaxFiles < l.MaxFiles || h.MaxChangedLines < l.MaxChangedLines {
				return fmt.Errorf("radar: author trust tier %d is stricter than tier %d", tier, tier-1)
			}
		}
	}
	return nil
}

// withTrustTier returns a copy of the policy with the adjustments of the
// highest configured tier at or below tier applied.
func (p PullRequestPolicy) withTrustTier(tier int) PullRequestPolicy {
	out := p
	out.AllowRules = append([]PullRequestPathRule(nil), p.AllowRules...)
	if p.AuthorTrust == nil {
		return out
	}
	var chosen *PullRequestTrustTier
	for i, t := range p.AuthorTrust.Tiers {
		if t.Tier <= tier && (chosen == nil || t.Tier > chosen.Tier) {
			chosen = &p.AuthorTrust.Tiers[i]
		}
	}
	if chosen == nil {
		return out
	}
	if chosen.MinReviewConfidence != 0 {
		out.MinReviewConfidence = chosen.MinReviewConfidence
	}
	for _, l := range chosen.RuleLimits {
		for i := range out.AllowRules {
			if out.AllowRules[i].Name == l.Rule {
				out.AllowRules[i].MaxFiles = l.MaxFiles
				out.AllowRules[i].MaxChangedLines = l.MaxChangedLines
			}
		}
	}
	return out
}

// ReviewWithAuthorTrust returns the live review, unchanged by trust, and a
// private audit of what the author's tier would have decided from the same
// review-agent verdict. The agent runs once.
func (r *PullRequestReviewer) ReviewWithAuthorTrust(in PullRequestInput, trust AuthorTrust) (PullRequestReview, AuthorTrustAudit) {
	live, facts := r.review(in)
	tier := trust.EffectiveTier()
	adjusted := r.policy.withTrustTier(tier)
	audit := AuthorTrustAudit{
		SchemaVersion: 1, RequestID: live.RequestID, HeadSHA: live.HeadSHA,
		PolicyVersion: live.PolicyVersion, Reviewer: live.Reviewer, Author: in.Author,
		Tier: trust.Tier, AgentAuthored: trust.AgentAuthored, EffectiveTier: tier,
		MinReviewConfidence: adjusted.MinReviewConfidence,
		LiveAction:          shadowAction(live.Action), TierAction: shadowAction(live.Action),
		MatchedRule: live.MatchedRule,
	}
	if !facts.agentReviewed {
		return live, audit
	}

	rule, allowReason := matchPullRequestRule(adjusted, in)
	allowlisted := rule != nil
	audit.Stages = append(audit.Stages, StageResult{Name: "trust.allow-rule", Passed: allowlisted, Reason: allowReason})
	audit.MatchedRule = ""
	if rule != nil {
		audit.MatchedRule = rule.Name
	}
	// The review prompt tells the model not to accept below the live minimum,
	// so below it a decline carries no information beyond the confidence.
	claimRequired := live.Agent.Confidence >= r.policy.MinReviewConfidence
	agentPassed := agentPasses(adjusted, live.Agent, facts.diff, claimRequired)
	audit.Stages = append(audit.Stages, StageResult{Name: "trust.review-agent", Passed: agentPassed,
		Reason: fmt.Sprintf("confidence=%d/%d claim-required=%t", live.Agent.Confidence, adjusted.MinReviewConfidence, claimRequired)})

	eligible := !facts.denied && allowlisted && facts.riskPassed
	audit.TierAction = shadowAction(decideAction(PullRequestModeShadow, facts.denied, eligible, agentPassed))
	audit.Adjusted = audit.TierAction != audit.LiveAction
	return live, audit
}

// shadowAction reports approvals as would-approve: a tier never acts.
func shadowAction(a PullRequestAction) PullRequestAction {
	if a == PullRequestApprove {
		return PullRequestWouldApprove
	}
	return a
}
