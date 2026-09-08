package radar

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadDRSModelFixture(t *testing.T, name string) *DRSModelScorer {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "drs_model", name))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newDRSModelScorerFromBytes(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

// validArtifactJSON is testdata/drs_model/valid.json with one edit applied, for
// fail-closed cases that need a single field changed.
func validArtifactJSON(t *testing.T, old, new string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "drs_model", "valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return []byte(strings.Replace(string(data), old, new, 1))
}

// TestDRSModelScorerGolden checks the dot product against hand-computed values
// for the valid fixture: intercept -2; lines_changed log1p standardized by
// (3, 2) with weight 1.5; risky_path_files standardized by (0.5, 0.5) with
// weight 2; test_files standardized by (1, 1) with weight -0.5.
func TestDRSModelScorerGolden(t *testing.T) {
	s := loadDRSModelFixture(t, "valid.json")
	if s.ModelVersion() != "drs-lr-test-0001" {
		t.Fatalf("model version = %q", s.ModelVersion())
	}
	tests := []struct {
		name string
		diff Diff
		want float64
	}{
		{
			// lines 19 -> log1p = ln 20 = 2.9957; (2.9957-3)/2*1.5 = -0.0032;
			// both files hit "auth" -> (2-0.5)/0.5*2 = 6; one test file -> 0.
			name: "two auth files one test",
			diff: Diff{Changes: []Change{
				{File: "src/auth/login.go", Type: "modified", Additions: 10, Deletions: 5},
				{File: "src/auth/login_test.go", Type: "added", Additions: 4},
			}},
			want: 3.9967992051654933,
		},
		{
			// all features zero: -2 + (0-3)/2*1.5 + (0-0.5)/0.5*2 + (0-1)/1*-0.5
			name: "empty diff",
			diff: Diff{},
			want: -5.75,
		},
		{
			// lines 3 -> log1p = ln 4; no risky path; no test file.
			name: "small doc change",
			diff: Diff{Changes: []Change{{File: "docs/setup.md", Type: "modified", Additions: 2, Deletions: 1}}},
			want: -4.710279229160082,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Score(tt.diff)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("Score = %.16g, want %.16g", got, tt.want)
			}
		})
	}
}

func TestDRSModelFeatures(t *testing.T) {
	patch := "@@ -1,3 +1,4 @@\n-old\n+new\n+more\n@@ -20,2 +21,2 @@\n-a\n+b"
	tests := []struct {
		feature string
		diff    Diff
		want    float64
	}{
		{"file_count", Diff{}, 0},
		{"file_count", Diff{Changes: []Change{{File: "a"}, {File: "b"}}}, 2},
		{"additions", Diff{Changes: []Change{{Additions: 3}, {Additions: 4, Deletions: 9}}}, 7},
		{"deletions", Diff{Changes: []Change{{Additions: 3}, {Additions: 4, Deletions: 9}}}, 9},
		{"lines_changed", Diff{}, 0},
		{"lines_changed", Diff{Changes: []Change{{Additions: 4, Deletions: 9}}}, 13},
		// Neither count set: newlines+1 of Content, exactly as LinesChanged.
		{"lines_changed", Diff{Changes: []Change{{Content: "a\nb\nc"}}}, 3},
		{"lines_changed", Diff{Changes: []Change{{Content: ""}}}, 1},
		{"max_file_lines", Diff{}, 0},
		{"max_file_lines", Diff{Changes: []Change{{Additions: 4, Deletions: 9}, {Additions: 20}, {Content: "a\nb"}}}, 20},
		{"max_file_lines", Diff{Changes: []Change{{Additions: 1}, {Content: "a\nb\nc\nd"}}}, 4},
		// A file matching three fragments still counts once.
		{"risky_path_files", Diff{Changes: []Change{{File: "src/Auth/security/config.go"}, {File: "src/util/format.go"}}}, 1},
		{"risky_path_files", Diff{Changes: []Change{{File: "db/migration/001.sql"}, {File: "payments/Billing.java"}}}, 2},
		{"risky_path_files", Diff{Changes: []Change{{File: "README.md"}}}, 0},
		{"renamed_files", Diff{Changes: []Change{{Type: "renamed"}, {Type: "Renamed"}, {Type: "modified"}}}, 2},
		{"added_files", Diff{Changes: []Change{{Type: "added"}, {Type: "removed"}}}, 1},
		{"removed_files", Diff{Changes: []Change{{Type: "added"}, {Type: "removed"}}}, 1},
		{"modified_files", Diff{Changes: []Change{{Type: "modified"}, {Type: "MODIFIED"}, {Type: ""}}}, 2},
		{"test_files", Diff{Changes: []Change{
			{File: "src/test/java/com/x/Foo.java"},
			{File: "tests/unit/foo.py"},
			{File: "pkg/foo_test.go"},
			{File: "pkg/test_foo.py"},
			{File: "web/foo.test.ts"},
			{File: "web/foo.spec.js"},
			{File: "src/main/java/com/x/FooTest.java"},
			{File: "src/main/java/com/x/FooTests.kt"},
		}}, 8},
		// Substrings that are not the conventions: no false positives.
		{"test_files", Diff{Changes: []Change{
			{File: "src/contest.go"},
			{File: "docs/latest.md"},
			{File: "src/testing/helpers.go"},
			{File: "attest.json"},
			{File: "src/Testing.java"},
		}}, 0},
		{"doc_files", Diff{Changes: []Change{{File: "README.md"}, {File: "docs/img/a.png"}, {File: "x/docs/y.txt"}, {File: "src/docsync.go"}, {File: "a/README.MD"}, {File: "bin/docs"}}}, 4},
		{"config_files", Diff{Changes: []Change{
			{File: "a.yaml"}, {File: "b.yml"}, {File: "c.json"}, {File: "d.toml"},
			{File: "e.properties"}, {File: "build.gradle"}, {File: "build.gradle.kts"}, {File: "f.go"}, {File: "yaml/g.txt"},
		}}, 7},
		{"hunk_count", Diff{Changes: []Change{{Content: patch}, {Content: patch}, {Content: "no hunks"}}}, 4},
		{"hunk_count", Diff{Changes: []Change{{Content: ""}}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.feature, func(t *testing.T) {
			fn, ok := drsModelFeatures[tt.feature]
			if !ok {
				t.Fatalf("feature %q not registered", tt.feature)
			}
			if got := fn(tt.diff); got != tt.want {
				t.Fatalf("%s(%+v) = %v, want %v", tt.feature, tt.diff.Changes, got, tt.want)
			}
		})
	}
}

// TestDRSModelFeatureNames pins the feature contract the Python training
// pipeline must match. Changing this list is a cross-repo change.
func TestDRSModelFeatureNames(t *testing.T) {
	want := []string{
		"added_files", "additions", "config_files", "deletions", "doc_files",
		"file_count", "hunk_count", "lines_changed", "max_file_lines",
		"modified_files", "removed_files", "renamed_files", "risky_path_files",
		"test_files",
	}
	got := DRSModelFeatureNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feature names = %v, want %v", got, want)
	}
	if len(got) != len(drsModelFeatures) {
		t.Fatalf("%d names for %d registered features", len(got), len(drsModelFeatures))
	}
}

func TestDRSModelScorerFailsClosed(t *testing.T) {
	fixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "drs_model", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"unknown feature", fixture("unknown_feature.json"), `unknown feature "cyclomatic_complexity"`},
		{"malformed json", fixture("malformed.json"), "decoding model artifact"},
		{"unknown transform", validArtifactJSON(t, `"transform": "log1p"`, `"transform": "sqrt"`), `unknown transform "sqrt"`},
		{"zero scale", validArtifactJSON(t, `"scale": 2.0`, `"scale": 0`), "scale must be positive"},
		{"negative scale", validArtifactJSON(t, `"scale": 2.0`, `"scale": -1`), "scale must be positive"},
		{"empty features", []byte(`{"schema_version":1,"model_version":"v","intercept":0,"features":[]}`), "no features"},
		{"missing features", []byte(`{"schema_version":1,"model_version":"v","intercept":0}`), "no features"},
		{"schema version 2", validArtifactJSON(t, `"schema_version": 1`, `"schema_version": 2`), "unsupported model schema_version 2"},
		{"schema version missing", []byte(`{"model_version":"v","intercept":0,"features":[{"name":"file_count","transform":"identity","mean":0,"scale":1,"coefficient":1}]}`), "unsupported model schema_version 0"},
		{"placeholder", []byte(`{"schema_version":1,"placeholder":true,"model_version":"","intercept":0,"features":[]}`), "placeholder"},
		{"placeholder with features", validArtifactJSON(t, `"schema_version": 1,`, `"schema_version": 1, "placeholder": true,`), "placeholder"},
		{"empty model version", validArtifactJSON(t, `"model_version": "drs-lr-test-0001"`, `"model_version": " "`), "model_version is required"},
		{"duplicate feature", validArtifactJSON(t, `"name": "test_files"`, `"name": "lines_changed"`), `feature "lines_changed" appears more than once`},
		{"unknown field", validArtifactJSON(t, `"intercept": -2.0,`, `"intercept": -2.0, "clip": true,`), "unknown field"},
		{"out of range number", validArtifactJSON(t, `"coefficient": 1.5`, `"coefficient": 1e999`), "decoding model artifact"},
		{"trailing object", append(fixture("valid.json"), []byte(`{}`)...), "exactly one JSON object"},
		{"empty input", nil, "decoding model artifact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := newDRSModelScorerFromBytes(tt.data)
			if err == nil {
				t.Fatalf("expected error, got scorer %+v", s)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// JSON cannot carry NaN or Inf, so the non-finite checks are exercised on the
// decoded artifact directly.
func TestDRSModelScorerRejectsNonFiniteParameters(t *testing.T) {
	feature := func() drsModelFeature {
		return drsModelFeature{Name: "file_count", Transform: "identity", Mean: 0, Scale: 1, Coefficient: 1}
	}
	valid := func() drsModelArtifact {
		return drsModelArtifact{SchemaVersion: 1, ModelVersion: "v", Features: []drsModelFeature{feature()}}
	}
	tests := []struct {
		name   string
		mutate func(*drsModelArtifact)
		want   string
	}{
		{"nan intercept", func(a *drsModelArtifact) { a.Intercept = math.NaN() }, "intercept must be finite"},
		{"inf intercept", func(a *drsModelArtifact) { a.Intercept = math.Inf(1) }, "intercept must be finite"},
		{"nan mean", func(a *drsModelArtifact) { a.Features[0].Mean = math.NaN() }, "must be finite"},
		{"inf scale", func(a *drsModelArtifact) { a.Features[0].Scale = math.Inf(1) }, "must be finite"},
		{"nan coefficient", func(a *drsModelArtifact) { a.Features[0].Coefficient = math.NaN() }, "must be finite"},
		{"neg inf coefficient", func(a *drsModelArtifact) { a.Features[0].Coefficient = math.Inf(-1) }, "must be finite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := valid()
			tt.mutate(&a)
			if _, err := newDRSModelScorer(a); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want mention of %q", err, tt.want)
			}
		})
	}
	if _, err := newDRSModelScorer(valid()); err != nil {
		t.Fatalf("baseline artifact should be valid: %v", err)
	}
}

func TestDRSModelScorerMonotonic(t *testing.T) {
	s := loadDRSModelFixture(t, "valid.json")
	small := Diff{Changes: []Change{{File: "src/util/format.go", Additions: 2}}}
	large := Diff{Changes: []Change{{File: "src/util/format.go", Additions: 200}}}
	risky := Diff{Changes: []Change{{File: "src/auth/format.go", Additions: 2}}}
	tested := Diff{Changes: []Change{{File: "src/util/format_test.go", Additions: 2}}}
	if s.Score(large) <= s.Score(small) {
		t.Error("more lines with a positive coefficient should score higher")
	}
	if s.Score(risky) <= s.Score(small) {
		t.Error("a risky path with a positive coefficient should score higher")
	}
	if s.Score(tested) >= s.Score(small) {
		t.Error("a test file with a negative coefficient should score lower")
	}
}

// TestDRSModelScorerScoreStaysFinite drives the linear term past the float
// range with a legal (finite) coefficient and checks Score clamps rather than
// returning Inf.
func TestDRSModelScorerScoreStaysFinite(t *testing.T) {
	s, err := newDRSModelScorer(drsModelArtifact{
		SchemaVersion: 1, ModelVersion: "overflow",
		Features: []drsModelFeature{{Name: "additions", Transform: "identity", Mean: 0, Scale: 1e-10, Coefficient: math.MaxFloat64}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Score(Diff{Changes: []Change{{Additions: 1000}}})
	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("Score = %v, want finite", got)
	}
	if got != math.MaxFloat64 {
		t.Fatalf("Score = %v, want MaxFloat64 (maximally risky)", got)
	}
	if NewCalibrator([]float64{0, 1, 2}).Percentile(got) != 100 {
		t.Fatal("clamped score must calibrate as maximally risky")
	}
}

func TestNewDRSModelScorerRefusesPlaceholder(t *testing.T) {
	s, err := NewDRSModelScorer()
	if err == nil {
		t.Fatalf("placeholder artifact must be refused, got %+v", s)
	}
	for _, want := range []string{"drs_model.json", "placeholder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewRiskScorer(t *testing.T) {
	s, err := NewRiskScorer(ScorerHeuristic)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(HeuristicScorer); !ok {
		t.Fatalf("heuristic scorer = %T", s)
	}
	if _, err := NewRiskScorer(ScorerDRSModel); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("drs-model must fail closed on the placeholder artifact, got %v", err)
	}
	if _, err := NewRiskScorer("bogus"); err == nil || !strings.Contains(err.Error(), `unknown scorer "bogus"`) {
		t.Fatalf("bogus scorer error = %v", err)
	}
	for _, name := range ScorerNames() {
		if _, err := NewRiskScorer(name); err != nil && !strings.Contains(err.Error(), "placeholder") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestScorerNames(t *testing.T) {
	if (HeuristicScorer{}).Name() != ScorerHeuristic {
		t.Fatal("HeuristicScorer.Name mismatch")
	}
	if loadDRSModelFixture(t, "valid.json").Name() != ScorerDRSModel {
		t.Fatal("DRSModelScorer.Name mismatch")
	}
}
