package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/travisjeffery/radar"
)

func TestQuantileSample(t *testing.T) {
	sorted := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	tests := []struct {
		points int
		want   []float64
	}{
		// k*(n-1)/(N-1) = k*9/4 = 0, 2.25, 4.5, 6.75, 9; 4.5 rounds away from zero.
		{5, []float64{0, 2, 5, 7, 9}},
		{2, []float64{0, 9}},
		{10, sorted},
		// More points than values repeat neighbours; ends stay min and max.
		// Index is round(k/2): k=1 gives 0.5 -> 1 (half away from zero, where
		// round-half-to-even would give 0).
		{19, []float64{0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9}},
	}
	for _, tt := range tests {
		got, err := quantileSample(sorted, tt.points)
		if err != nil {
			t.Fatalf("points=%d: %v", tt.points, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("points=%d: got %v, want %v", tt.points, got, tt.want)
		}
	}
	if got, err := quantileSample([]float64{3.5}, 3); err != nil || !reflect.DeepEqual(got, []float64{3.5, 3.5, 3.5}) {
		t.Errorf("single value: got %v, %v", got, err)
	}
	if _, err := quantileSample(nil, 5); err == nil {
		t.Error("empty input must error")
	}
	if _, err := quantileSample(sorted, 1); err == nil {
		t.Error("fewer than two points must error")
	}
}

func TestQuantileIndex(t *testing.T) {
	for _, tt := range []struct {
		q    float64
		n    int
		want int
	}{
		{0, 10, 0}, {1, 10, 9}, {0.5, 10, 5}, {0.9, 10, 8}, {0.5, 1, 0}, {0.5, 2, 1},
	} {
		if got := quantileIndex(tt.q, tt.n); got != tt.want {
			t.Errorf("quantileIndex(%v, %d) = %d, want %d", tt.q, tt.n, got, tt.want)
		}
	}
}

func TestLoadCalibrationSample(t *testing.T) {
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "sample.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	got, err := loadCalibrationSample(write(`[-1.5, 0, 2]`))
	if err != nil || !reflect.DeepEqual(got, []float64{-1.5, 0, 2}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for name, content := range map[string]string{"empty": `[]`, "object": `{"a":1}`, "text": `nope`, "huge": `[1e999]`} {
		if _, err := loadCalibrationSample(write(content)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := loadCalibrationSample(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing file must error")
	}
}

// TestClassifyModelScorerRequiresCalibration guards the fail-open case where a
// model's logits would be calibrated against the heuristic's synthetic sample.
func TestClassifyModelScorerRequiresCalibration(t *testing.T) {
	old := os.Stderr
	path := filepath.Join(t.TempDir(), "stderr")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = file
	code := runClassify([]string{"-scorer", "drs-model", filepath.Join("..", "..", "testdata", "human_approved.json")})
	os.Stderr = old
	_ = file.Close()
	data, _ := os.ReadFile(path)
	if code != 2 || !strings.Contains(string(data), "requires -calibration") {
		t.Fatalf("code=%d stderr=%q", code, data)
	}
}

// captureStdout runs f with os.Stdout redirected to a file and returns what
// was written.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = old }()
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

// TestDRSFeatureRows pins exact values for the fixture so a reimplementation
// diffing against `radar drs-features testdata/drs_features.json` has a
// known-good reference.
func TestDRSFeatureRows(t *testing.T) {
	diffs, err := loadDiffs(filepath.Join("..", "..", "testdata", "drs_features.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows := drsFeatureRows(diffs)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	want := map[string]map[string]float64{
		// F1: 4 files; lines 15+4+2+30 = 51; two "auth" paths plus docs/auth.md = 3;
		// one test file; docs/auth.md is a doc; values.yaml is config; 5 hunks.
		"F1": {
			"file_count": 4, "additions": 15, "deletions": 36, "lines_changed": 51, "max_file_lines": 30,
			"risky_path_files": 3, "added_files": 1, "removed_files": 1, "modified_files": 1, "renamed_files": 1,
			"test_files": 1, "doc_files": 1, "config_files": 1, "hunk_count": 5,
		},
		// F2: no provider counts, so lines fall back to newlines+1 of content.
		"F2": {
			"file_count": 1, "additions": 0, "deletions": 0, "lines_changed": 3, "max_file_lines": 3,
			"risky_path_files": 0, "added_files": 0, "removed_files": 0, "modified_files": 0, "renamed_files": 0,
			"test_files": 0, "doc_files": 0, "config_files": 0, "hunk_count": 0,
		},
	}
	names := radar.DRSModelFeatureNames()
	for _, row := range rows {
		if len(row.Features) != len(names) {
			t.Fatalf("%s: %d features, want %d", row.ID, len(row.Features), len(names))
		}
		for _, name := range names {
			if _, ok := row.Features[name]; !ok {
				t.Fatalf("%s: missing feature %q", row.ID, name)
			}
		}
		expected, ok := want[row.ID]
		if !ok {
			continue
		}
		for name, value := range expected {
			if got := row.Features[name]; got != value {
				t.Errorf("%s %s = %v, want %v", row.ID, name, got, value)
			}
		}
	}
	// F3 has no changes at all: every feature is zero.
	for name, value := range rows[2].Features {
		if value != 0 {
			t.Errorf("F3 %s = %v, want 0", name, value)
		}
	}
}

func TestRunDRSFeatures(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = runDRSFeatures([]string{"-names"}) })
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil || code != 0 {
		t.Fatalf("code=%d out=%q err=%v", code, out, err)
	}
	if !reflect.DeepEqual(names, radar.DRSModelFeatureNames()) {
		t.Fatalf("names = %v", names)
	}

	out = captureStdout(t, func() {
		code = runDRSFeatures([]string{filepath.Join("..", "..", "testdata", "drs_features.json")})
	})
	var rows []drsFeatureRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil || code != 0 {
		t.Fatalf("code=%d out=%q err=%v", code, out, err)
	}
	if len(rows) != 3 || rows[0].ID != "F1" || rows[0].Features["lines_changed"] != 51 || rows[1].Features["max_file_lines"] != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	// Keys are emitted sorted, so the golden is byte-stable across runs.
	if !strings.Contains(out, "\"added_files\": 1,\n      \"additions\": 15,") {
		t.Fatalf("features must be emitted in sorted order: %q", out)
	}
}
