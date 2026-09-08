package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
