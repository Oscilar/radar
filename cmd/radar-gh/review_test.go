package main

import (
	"encoding/json"
	"os"
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
	changed.StaleApprovalsDismissed = false
	if err := validateApprovalSnapshot(base, changed, head); err == nil {
		t.Fatal("stale approval protection must block approval")
	}
}

func TestCheckScorerCalibration(t *testing.T) {
	tests := []struct {
		calibratedFor string
		scorer        string
		wantErr       bool
	}{
		{"", radar.ScorerHeuristic, false},
		{radar.ScorerHeuristic, radar.ScorerHeuristic, false},
		{radar.ScorerDRSModel, radar.ScorerDRSModel, false},
		// A legacy policy without calibrated_for was calibrated for the heuristic.
		{"", radar.ScorerDRSModel, true},
		{radar.ScorerHeuristic, radar.ScorerDRSModel, true},
		{radar.ScorerDRSModel, radar.ScorerHeuristic, true},
	}
	for _, tt := range tests {
		policy := radar.PullRequestPolicy{CalibratedFor: tt.calibratedFor}
		err := checkScorerCalibration(policy, tt.scorer)
		if (err != nil) != tt.wantErr {
			t.Errorf("calibrated_for=%q scorer=%q: err=%v, wantErr=%t", tt.calibratedFor, tt.scorer, err, tt.wantErr)
		}
		if err != nil && (!strings.Contains(err.Error(), tt.scorer) || !strings.Contains(err.Error(), "calibrated_for")) {
			t.Errorf("error %q should name the scorer and calibrated_for", err)
		}
	}
}

func writeTestPolicy(t *testing.T, policy radar.PullRequestPolicy) string {
	t.Helper()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPullRequestPolicyCalibratedFor(t *testing.T) {
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.CalibratedFor != radar.ScorerHeuristic {
		t.Fatalf("example calibrated_for = %q, want heuristic", policy.CalibratedFor)
	}
	policy.CalibratedFor = radar.ScorerDRSModel
	reloaded, err := loadPullRequestPolicy(writeTestPolicy(t, policy))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CalibratedFor != radar.ScorerDRSModel {
		t.Fatalf("calibrated_for = %q", reloaded.CalibratedFor)
	}
	policy.CalibratedFor = "bogus"
	if _, err := loadPullRequestPolicy(writeTestPolicy(t, policy)); err == nil {
		t.Fatal("unknown calibrated_for must fail to load")
	}
}

// captureStderr runs f with os.Stderr redirected to a file and returns what
// was written.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = old }()
	f()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRunReviewRefusesMismatchedScorer exercises runReview itself: with a
// policy calibrated for another scorer it must exit 1 before building an agent
// (no API key is set) or touching GitHub (PATH is empty, so gh cannot run).
func TestRunReviewRefusesMismatchedScorer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	policy, err := loadPullRequestPolicy(filepath.Join("..", "..", "examples", "github-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy.CalibratedFor = radar.ScorerDRSModel
	path := writeTestPolicy(t, policy)

	var code int
	stderr := captureStderr(t, func() {
		code = runReview([]string{"-repo", "owner/repo", "-pr", "1", "-policy", path, "-scorer", radar.ScorerHeuristic})
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `calibrated_for) but -scorer is "heuristic"`) {
		t.Fatalf("stderr = %q", stderr)
	}
	for _, later := range []string{"OPENAI_API_KEY", "executable file not found", "fetching pull request"} {
		if strings.Contains(stderr, later) {
			t.Fatalf("guard must run before agent construction and GitHub calls: %q", stderr)
		}
	}
}

func TestRunReviewRejectsUnknownScorer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join("..", "..", "examples", "github-policy.json")
	var code int
	stderr := captureStderr(t, func() {
		code = runReview([]string{"-repo", "owner/repo", "-pr", "1", "-policy", path, "-agent", "rule-based", "-scorer", "bogus"})
	})
	if code != 1 || !strings.Contains(stderr, "calibrated_for") || !strings.Contains(stderr, `"bogus"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
