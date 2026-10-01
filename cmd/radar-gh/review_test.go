package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func TestExamplePolicyLoadsInShadowMode(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != radar.PullRequestModeShadow {
		t.Fatalf("example mode = %q, want shadow", policy.Mode)
	}
}

func TestEvaluateChecks(t *testing.T) {
	policy := radar.PullRequestPolicy{IgnoredChecks: []string{"github-actions/review"}}
	runs := []githubCheckRun{
		{ID: 1, Name: "test", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "test", Status: "completed", Conclusion: "success"},
		{ID: 3, Name: "review", Status: "in_progress"},
	}
	runs[0].App.Slug = "github-actions"
	runs[1].App.Slug = "github-actions"
	runs[2].App.Slug = "github-actions"
	got := evaluateChecks(runs, nil, policy)
	if !got.Observed || !got.Passing || got.Fingerprint == "" {
		t.Fatalf("got %+v", got)
	}

	policy.IgnoredChecks = nil
	got = evaluateChecks(runs, nil, policy)
	if got.Passing {
		t.Fatal("in-progress self check must fail unless explicitly ignored")
	}

	got = evaluateChecks(nil, nil, policy)
	if got.Observed || got.Passing {
		t.Fatal("no checks must fail closed")
	}
}

func TestEvaluateChecksSkippedIsExplicit(t *testing.T) {
	run := githubCheckRun{ID: 1, Name: "optional", Status: "completed", Conclusion: "skipped"}
	run.App.Slug = "github-actions"
	policy := radar.PullRequestPolicy{}
	if evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check must fail by default")
	}
	policy.AllowSkippedChecks = true
	if !evaluateChecks([]githubCheckRun{run}, nil, policy).Passing {
		t.Fatal("skipped check should pass only when explicitly allowed")
	}
}

func TestPatchLineCountsMatch(t *testing.T) {
	patch := "@@ -1,2 +1,2 @@\n-old\n context\n+new"
	if !patchLineCountsMatch(patch, 1, 1) {
		t.Fatal("complete patch should match")
	}
	if patchLineCountsMatch(patch, 2, 1) {
		t.Fatal("truncated patch must not match provider totals")
	}
}

func TestExistingRadarApproval(t *testing.T) {
	changes := githubReview{ID: 1, State: "CHANGES_REQUESTED"}
	changes.User.Login = "reviewer"
	approved := githubReview{ID: 2, State: "APPROVED"}
	approved.User.Login = "reviewer"
	approved.User.Type = "Bot"
	approved.CommitID = strings.Repeat("a", 40)
	approved.Body = radarApprovalPrefix + "1 approved this exact head"
	existing := existingRadarApproval([]githubReview{changes, approved}, approved.CommitID)
	if !existing {
		t.Fatal("current Radar approval was not found")
	}
	if existingRadarApproval([]githubReview{approved}, strings.Repeat("b", 40)) {
		t.Fatal("approval for an old head must not be treated as current")
	}
}

func TestValidateApprovalSnapshot(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := radar.PullRequestInput{
		HeadSHA: head, CheckFingerprint: "stable", ChecksObserved: true, ChecksPassing: true,
		Open: true, SameRepository: true, StaleApprovalsDismissed: true,
	}
	if err := validateApprovalSnapshot(base, base, head); err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.CheckFingerprint = "changed"
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("changed checks must block approval")
	}
	changed = base
	changed.BaseRef = "someone/feature"
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("a retarget after review must block approval")
	}
	changed = base
	changed.Stack = []radar.PullRequestStackEntry{{Number: 1, HeadRef: base.BaseRef, HeadSHA: head, BaseRef: "main"}}
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("a stacked pull request must never be approved")
	}
	changed = base
	changed.StaleApprovalsDismissed = false
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("stale approval protection must block approval")
	}
}

func TestNewReviewAgentFireworks(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "")
	t.Setenv("RADAR_ACR_MODEL", "")
	if _, err := newReviewAgents("fireworks"); err == nil || !strings.Contains(err.Error(), "FIREWORKS_API_KEY") {
		t.Fatalf("missing key must error, got %v", err)
	}
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	if _, err := newReviewAgents("fireworks"); err == nil || !strings.Contains(err.Error(), "RADAR_ACR_MODEL") {
		t.Fatalf("missing model must error, got %v", err)
	}
	t.Setenv("RADAR_ACR_MODEL", "accounts/fireworks/models/glm-5p3")
	agents, err := newReviewAgents("fireworks")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agents[0].(*radar.FireworksAgent); !ok {
		t.Fatalf("agent = %T, want *radar.FireworksAgent", agents[0])
	}
	if _, err := newReviewAgents("bogus"); err == nil {
		t.Fatal("unknown agent must error")
	}
}

func TestNewReviewAgentsSeveral(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("FIREWORKS_API_KEY", "fireworks-key")
	t.Setenv("RADAR_ACR_MODEL", "accounts/fireworks/models/glm-5p3")
	if _, err := newReviewAgents("openai,fireworks=accounts/fireworks/models/glm-5p3"); err == nil {
		t.Fatal("RADAR_ACR_MODEL must be unset with several agents")
	}
	t.Setenv("RADAR_ACR_MODEL", "")
	agents, err := newReviewAgents("openai, fireworks=accounts/fireworks/models/glm-5p3")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if got := agents[0].(*radar.OpenAIAgent).Model; got != "gpt-4o-mini" {
		t.Fatalf("openai model = %q, want its default", got)
	}
	if got := agents[1].(*radar.FireworksAgent).Model; got != "accounts/fireworks/models/glm-5p3" {
		t.Fatalf("fireworks model = %q", got)
	}
	if _, err := newReviewAgents("openai,openai"); err == nil {
		t.Fatal("duplicate agents must error")
	}
}

func stubPulls(t *testing.T, byHead map[string]string) *[]string {
	t.Helper()
	var calls []string
	prev := runGH
	t.Cleanup(func() { runGH = prev })
	runGH = func(args ...string) ([]byte, error) {
		endpoint := args[len(args)-1]
		calls = append(calls, endpoint)
		for head, body := range byHead {
			if strings.Contains(endpoint, "head="+url.QueryEscape("Oscilar:"+head)+"&") {
				return []byte(body), nil
			}
		}
		return []byte("[]"), nil
	}
	return &calls
}

func pullJSON(number int, head, base, headRepo string) string {
	return fmt.Sprintf(`{"number":%d,"head":{"ref":%q,"sha":"%040d","repo":{"full_name":%q}},"base":{"ref":%q,"repo":{"full_name":"Oscilar/backend"}}}`,
		number, head, number, headRepo, base)
}

func TestFetchStack(t *testing.T) {
	stubPulls(t, map[string]string{
		"ram/wf-1631": "[" + pullJSON(14623, "ram/wf-1631", "ram/wf-1630", "Oscilar/backend") + "]",
		"ram/wf-1630": "[" + pullJSON(14622, "ram/wf-1630", "main", "Oscilar/backend") + "]",
	})
	stack, err := fetchStack("Oscilar/backend", "ram/wf-1631", []string{"main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stack) != 2 || stack[0].Number != 14623 || stack[1].Number != 14622 || stack[1].BaseRef != "main" {
		t.Fatalf("stack = %+v", stack)
	}
}

func TestFetchStackRejectsForksAndAmbiguity(t *testing.T) {
	stubPulls(t, map[string]string{
		"fork-branch": "[" + pullJSON(1, "fork-branch", "main", "someone/backend") + "]",
		"shared":      "[" + pullJSON(2, "shared", "main", "Oscilar/backend") + "," + pullJSON(3, "shared", "release", "Oscilar/backend") + "]",
	})
	for _, base := range []string{"fork-branch", "shared", "no-such-branch"} {
		stack, err := fetchStack("Oscilar/backend", base, []string{"main"})
		if err != nil || stack != nil {
			t.Fatalf("base %s: stack=%+v err=%v, want no stack", base, stack, err)
		}
	}
}

func TestFetchStackIsBounded(t *testing.T) {
	pulls := map[string]string{}
	for i := range maxStackDepth + 2 {
		pulls[fmt.Sprintf("b%d", i)] = "[" + pullJSON(i+1, fmt.Sprintf("b%d", i), fmt.Sprintf("b%d", i+1), "Oscilar/backend") + "]"
	}
	calls := stubPulls(t, pulls)
	stack, err := fetchStack("Oscilar/backend", "b0", []string{"main"})
	if err != nil || stack != nil || len(*calls) != maxStackDepth {
		t.Fatalf("stack=%+v err=%v calls=%d", stack, err, len(*calls))
	}
}

func TestExampleReviewFindingsPolicyLoads(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy-review-findings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.ApprovalBasis != radar.ApprovalBasisReviewFindings || policy.Mode != radar.PullRequestModeShadow {
		t.Fatalf("policy = %+v", policy)
	}
}
