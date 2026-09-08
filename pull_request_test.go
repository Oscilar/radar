package radar

import (
	"math"
	"strings"
	"testing"
)

type fixedScorer float64

func (s fixedScorer) Score(Diff) float64 { return float64(s) }

type countingAgent struct {
	result ACRResult
	calls  int
}

func (a *countingAgent) Review(Diff) ACRResult {
	a.calls++
	return a.result
}

func testPullRequestPolicy(mode PullRequestMode) PullRequestPolicy {
	return PullRequestPolicy{
		Version:             1,
		Mode:                mode,
		AllowedBaseBranches: []string{"main"},
		AllowRules: []PullRequestPathRule{{
			Name:            "documentation",
			Include:         []string{"**/*.md"},
			Exclude:         []string{".github/**", "docs/runbooks/**"},
			Statuses:        []string{"added", "modified"},
			MaxFiles:        3,
			MaxChangedLines: 20,
		}},
		DenyPaths:           []string{".github/**", "**/security/**"},
		DenyPhrases:         []string{"disable verification"},
		MaxFiles:            5,
		MaxChangedLines:     40,
		MaxRiskPercentile:   20,
		CalibrationSample:   []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		MinReviewConfidence: 10,
	}
}

func safePullRequestInput() PullRequestInput {
	return PullRequestInput{
		ID: "owner/repo#42", Repository: "owner/repo", Number: 42,
		Title: "Clarify setup", BaseRef: "main", HeadSHA: strings.Repeat("a", 40),
		Author: "contributor", Open: true, SameRepository: true,
		ChecksObserved: true, ChecksStable: true, ChecksPassing: true,
		CheckFingerprint: "stable", StaleApprovalsDismissed: true,
		Files: []PullRequestFile{{
			Path: "docs/setup.md", Status: "modified", Additions: 2, Deletions: 1,
			Patch: "@@ -1 +1 @@\n-old\n+new", ContentComplete: true,
		}},
	}
}

func safeAgentResult() ACRResult {
	return ACRResult{
		Accept: true, Confidence: 10,
		SafeSignals: []ChangeSignal{SignalDocCommentUpdate}, ReviewedFiles: []string{"docs/setup.md"},
		Summary: "documentation-only change",
	}
}

func TestPullRequestReviewerShadowWouldApprove(t *testing.T) {
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.StaleApprovalsDismissed = false
	got := reviewer.Review(in)
	if got.Action != PullRequestWouldApprove || !got.Eligible || got.MatchedRule != "documentation" {
		t.Fatalf("got %+v", got)
	}
	if agent.calls != 1 {
		t.Fatalf("agent calls = %d, want 1", agent.calls)
	}
}

func TestPullRequestReviewerApprovalRequiresStaleDismissal(t *testing.T) {
	policy := testPullRequestPolicy(PullRequestModeApprove)
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(policy, fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.StaleApprovalsDismissed = false
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman || agent.calls != 0 {
		t.Fatalf("must fail before LLM: review=%+v calls=%d", got, agent.calls)
	}
	in.StaleApprovalsDismissed = true
	got = reviewer.Review(in)
	if got.Action != PullRequestApprove {
		t.Fatalf("got action %q, want approve", got.Action)
	}
}

func TestPullRequestReviewerConservativeRouting(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*PullRequestInput)
		score      float64
		agent      ACRResult
		want       PullRequestAction
		agentCalls int
	}{
		{name: "draft", mutate: func(in *PullRequestInput) { in.Draft = true }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "fork", mutate: func(in *PullRequestInput) { in.SameRepository = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "unstable checks", mutate: func(in *PullRequestInput) { in.ChecksStable = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "unresolved thread", mutate: func(in *PullRequestInput) { in.UnresolvedThreads = 1 }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "changes requested", mutate: func(in *PullRequestInput) { in.ChangesRequested = true }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "incomplete patch", mutate: func(in *PullRequestInput) { in.Files[0].ContentComplete = false }, agent: safeAgentResult(), want: PullRequestRouteToHuman},
		{name: "allow miss becomes candidate", mutate: func(in *PullRequestInput) { in.Files[0].Path = "src/main.go" }, agent: safeAgentResult(), want: PullRequestPolicyCandidate, agentCalls: 1},
		{name: "high score becomes candidate", mutate: func(in *PullRequestInput) {}, score: 9, agent: safeAgentResult(), want: PullRequestPolicyCandidate, agentCalls: 1},
		{name: "deny path never candidate", mutate: func(in *PullRequestInput) { in.Files[0].Path = ".github/workflows/ci.md" }, agent: safeAgentResult(), want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "denied phrase never candidate", mutate: func(in *PullRequestInput) { in.Body = "disable verification" }, agent: safeAgentResult(), want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "agent veto", mutate: func(in *PullRequestInput) {}, agent: ACRResult{Confidence: 10, RiskSignals: []ChangeSignal{SignalBugOrLogicError}, Summary: "risk"}, want: PullRequestRouteToHuman, agentCalls: 1},
		{name: "incomplete agent coverage", mutate: func(in *PullRequestInput) {}, agent: ACRResult{Accept: true, Confidence: 10, SafeSignals: []ChangeSignal{SignalDocCommentUpdate}, Summary: "safe"}, want: PullRequestRouteToHuman, agentCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &countingAgent{result: tt.agent}
			reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(tt.score), agent)
			if err != nil {
				t.Fatal(err)
			}
			in := safePullRequestInput()
			tt.mutate(&in)
			if len(agent.result.ReviewedFiles) == 1 {
				agent.result.ReviewedFiles[0] = in.Files[0].Path
			}
			got := reviewer.Review(in)
			if got.Action != tt.want {
				t.Fatalf("action = %q, want %q; stages=%+v", got.Action, tt.want, got.Stages)
			}
			if agent.calls != tt.agentCalls {
				t.Fatalf("agent calls = %d, want %d", agent.calls, tt.agentCalls)
			}
		})
	}
}

func TestPullRequestRenameChecksOldAndNewPaths(t *testing.T) {
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(0), agent)
	if err != nil {
		t.Fatal(err)
	}
	in := safePullRequestInput()
	in.Files[0].Status = "renamed"
	in.Files[0].PreviousPath = "src/security/policy.md"
	got := reviewer.Review(in)
	if got.Action != PullRequestRouteToHuman {
		t.Fatalf("old denied path must block candidate and approval: %+v", got)
	}
}

func TestPullRequestPolicyValidate(t *testing.T) {
	policy := testPullRequestPolicy(PullRequestModeShadow)
	policy.CalibrationSample = nil
	if err := policy.Validate(); err == nil {
		t.Fatal("expected missing calibration error")
	}
	policy = testPullRequestPolicy(PullRequestModeShadow)
	policy.AllowRules[0].Include = []string{"../secret"}
	if err := policy.Validate(); err == nil {
		t.Fatal("expected invalid glob error")
	}
}

func TestPathGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/setup.md", true},
		{"docs/*", "docs/setup.md", true},
		{"docs/*", "docs/guides/setup.md", false},
		{"release/?otes.md", "release/notes.md", true},
	} {
		got := matchesAnyPath(tc.path, []string{tc.pattern})
		if got != tc.want {
			t.Errorf("matchesAnyPath(%q, %q) = %t, want %t", tc.path, tc.pattern, got, tc.want)
		}
	}
}

// TestPullRequestReviewerWithModelScorer runs the fixture model end to end. The
// safe input scores -4.7103 (see TestDRSModelScorerGolden); two of the ten
// calibration values lie strictly below it, so the percentile is exactly 20 and
// passes a P20 threshold.
func TestPullRequestReviewerWithModelScorer(t *testing.T) {
	scorer := loadDRSModelFixture(t, "valid.json")
	policy := testPullRequestPolicy(PullRequestModeShadow)
	policy.CalibratedFor = ScorerDRSModel
	policy.CalibrationSample = []float64{-6, -5, -4, -3, -2, -1, 0, 1, 2, 3}
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(policy, scorer, agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(safePullRequestInput())
	if got.Action != PullRequestWouldApprove || got.RiskPercentile != 20 {
		t.Fatalf("got %+v", got)
	}
	if got.ModelVersion != "drs-lr-test-0001" {
		t.Fatalf("model version = %q", got.ModelVersion)
	}
	var stage StageResult
	for _, s := range got.Stages {
		if s.Name == "pr.risk-threshold" {
			stage = s
		}
	}
	if !stage.Passed || !strings.Contains(stage.Reason, "scorer drs-model drs-lr-test-0001") {
		t.Fatalf("risk stage = %+v", stage)
	}

	// With three sample values below the same score the percentile is 30, the
	// gate fails, and the safe verdict is only a policy-update candidate: the
	// outcome is decided by the calibration sample, not the raw logit.
	policy.CalibrationSample = []float64{-9, -8, -7, -3, -2, -1, 0, 1, 2, 3}
	reviewer, err = NewPullRequestReviewer(policy, scorer, agent)
	if err != nil {
		t.Fatal(err)
	}
	if got := reviewer.Review(safePullRequestInput()); got.Action != PullRequestPolicyCandidate || got.RiskPercentile != 30 || got.ModelVersion != "drs-lr-test-0001" {
		t.Fatalf("got %+v", got)
	}
}

func TestPullRequestReviewerHeuristicHasNoModelVersion(t *testing.T) {
	agent := &countingAgent{result: safeAgentResult()}
	reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), HeuristicScorer{}, agent)
	if err != nil {
		t.Fatal(err)
	}
	got := reviewer.Review(safePullRequestInput())
	if got.ModelVersion != "" {
		t.Fatalf("model version = %q, want empty", got.ModelVersion)
	}
	found := false
	for _, s := range got.Stages {
		if s.Name == "pr.risk-threshold" && strings.Contains(s.Reason, "(scorer heuristic)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("risk stage should name the heuristic scorer: %+v", got.Stages)
	}
}

func TestPullRequestReviewerNonFiniteScoreFailsClosed(t *testing.T) {
	for _, score := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		agent := &countingAgent{result: safeAgentResult()}
		reviewer, err := NewPullRequestReviewer(testPullRequestPolicy(PullRequestModeShadow), fixedScorer(score), agent)
		if err != nil {
			t.Fatal(err)
		}
		got := reviewer.Review(safePullRequestInput())
		if got.Action != PullRequestRouteToHuman || got.Eligible || got.RiskPercentile != -1 {
			t.Fatalf("score %v: got %+v", score, got)
		}
		last := got.Stages[len(got.Stages)-1]
		if last.Name != "pr.risk-threshold" || last.Passed || !strings.Contains(last.Reason, "non-finite") {
			t.Fatalf("score %v: last stage = %+v", score, last)
		}
		if agent.calls != 0 {
			t.Fatalf("score %v: agent must not run after a non-finite score, calls=%d", score, agent.calls)
		}
	}
}

func TestPullRequestPolicyValidateCalibratedFor(t *testing.T) {
	for _, name := range []string{"", ScorerHeuristic, ScorerDRSModel} {
		policy := testPullRequestPolicy(PullRequestModeShadow)
		policy.CalibratedFor = name
		if err := policy.Validate(); err != nil {
			t.Errorf("calibrated_for %q: %v", name, err)
		}
	}
	for _, name := range []string{"bogus", "Heuristic ", "xgboost"} {
		policy := testPullRequestPolicy(PullRequestModeShadow)
		policy.CalibratedFor = name
		if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "calibrated_for") {
			t.Errorf("calibrated_for %q: error = %v", name, err)
		}
	}
}
