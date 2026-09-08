package radar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// DRSModelScorer is a RiskScorer backed by a trained linear model — the
// decision function of a logistic regression — over named diff features. The
// model artifact (see drsModelArtifact) is produced by an external training
// pipeline and embedded in the binary; it names WHICH features to use and their
// standardization and weights, while this package is the source of truth for
// HOW each named feature is computed from a Diff (see drsModelFeatures).
//
// Score returns the raw logit: intercept + Σ coefficient·((transform(x) − mean)
// / scale). Higher is riskier, which is all a Calibrator needs; the calibration
// sample must have been computed with this same scorer.
type DRSModelScorer struct {
	version   string
	intercept float64
	terms     []drsModelTerm
}

type drsModelTerm struct {
	feature     func(Diff) float64
	transform   func(float64) float64
	mean        float64
	scale       float64
	coefficient float64
}

// drsModelArtifact is the JSON contract with the training pipeline. Unknown
// fields are rejected so a semantic the pipeline adds is never silently
// dropped on the scoring side.
type drsModelArtifact struct {
	SchemaVersion int               `json:"schema_version"`
	Placeholder   bool              `json:"placeholder,omitempty"`
	ModelVersion  string            `json:"model_version"`
	TrainedAt     string            `json:"trained_at,omitempty"`
	TrainingRun   string            `json:"training_run,omitempty"`
	Intercept     float64           `json:"intercept"`
	Features      []drsModelFeature `json:"features"`
}

type drsModelFeature struct {
	Name        string  `json:"name"`
	Transform   string  `json:"transform"`
	Mean        float64 `json:"mean"`
	Scale       float64 `json:"scale"`
	Coefficient float64 `json:"coefficient"`
}

// drsModelSchemaVersion is the only artifact schema this build understands.
const drsModelSchemaVersion = 1

var drsModelTransforms = map[string]func(float64) float64{
	"identity": func(x float64) float64 { return x },
	"log1p":    math.Log1p,
}

// drsModelFeatures is the registry of features a model artifact may name. Every
// feature is derived from the fields the production adapter populates on each
// Change (File, PreviousFile, Type, Additions, Deletions, Content); Complexity
// and Signals are never set on that path and so are deliberately absent here.
// The Python training pipeline must reproduce these rules exactly.
var drsModelFeatures = map[string]func(Diff) float64{
	// file_count: number of changed files.
	"file_count": func(d Diff) float64 { return float64(len(d.Changes)) },
	// additions: sum of provider-reported added lines.
	"additions": func(d Diff) float64 { return float64(sumChanges(d, func(c Change) int { return c.Additions })) },
	// deletions: sum of provider-reported deleted lines.
	"deletions": func(d Diff) float64 { return float64(sumChanges(d, func(c Change) int { return c.Deletions })) },
	// lines_changed: Diff.LinesChanged — additions+deletions per file, falling back to newlines+1 of Content when both are zero.
	"lines_changed": func(d Diff) float64 { return float64(d.LinesChanged()) },
	// max_file_lines: the largest single file's line count under the lines_changed rule.
	"max_file_lines": func(d Diff) float64 {
		largest := 0
		for _, c := range d.Changes {
			largest = max(largest, c.lines())
		}
		return float64(largest)
	},
	// risky_path_files: files whose lowercased path contains any riskyPathFragments entry, counted once per file.
	"risky_path_files": func(d Diff) float64 { return float64(countChanges(d, isRiskyPath)) },
	// renamed_files: files whose change type is "renamed" (case-insensitive).
	"renamed_files": func(d Diff) float64 { return float64(countChanges(d, hasType("renamed"))) },
	// added_files: files whose change type is "added" (case-insensitive).
	"added_files": func(d Diff) float64 { return float64(countChanges(d, hasType("added"))) },
	// removed_files: files whose change type is "removed" (case-insensitive).
	"removed_files": func(d Diff) float64 { return float64(countChanges(d, hasType("removed"))) },
	// modified_files: files whose change type is "modified" (case-insensitive).
	"modified_files": func(d Diff) float64 { return float64(countChanges(d, hasType("modified"))) },
	// test_files: files matching isTestPath.
	"test_files": func(d Diff) float64 { return float64(countChanges(d, isTestPath)) },
	// doc_files: files whose lowercased path ends in ".md" or has a "docs" directory segment.
	"doc_files": func(d Diff) float64 { return float64(countChanges(d, isDocPath)) },
	// config_files: files whose lowercased path ends in .yaml, .yml, .json, .toml, .properties, .gradle, or .kts.
	"config_files": func(d Diff) float64 { return float64(countChanges(d, isConfigPath)) },
	// hunk_count: lines beginning with "@@" (unified-diff hunk headers) across all Content.
	"hunk_count": func(d Diff) float64 { return float64(sumChanges(d, countHunkHeaders)) },
}

// DRSModelFeatureNames returns the sorted names of every feature a model
// artifact may reference. The training pipeline's feature list must be a
// subset of this list.
func DRSModelFeatureNames() []string {
	names := make([]string, 0, len(drsModelFeatures))
	for name := range drsModelFeatures {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DRSModelFeatureValues computes every registered feature for the diff, keyed
// by name. It is the golden for a reimplementation of the feature rules: a
// training pipeline that reproduces these values feeds the model exactly what
// DRSModelScorer.Score does.
func DRSModelFeatureValues(d Diff) map[string]float64 {
	values := make(map[string]float64, len(drsModelFeatures))
	for name, feature := range drsModelFeatures {
		values[name] = feature(d)
	}
	return values
}

func sumChanges(d Diff, f func(Change) int) int {
	total := 0
	for _, c := range d.Changes {
		total += f(c)
	}
	return total
}

func countChanges(d Diff, pred func(Change) bool) int {
	n := 0
	for _, c := range d.Changes {
		if pred(c) {
			n++
		}
	}
	return n
}

func hasType(want string) func(Change) bool {
	return func(c Change) bool { return strings.EqualFold(c.Type, want) }
}

func isRiskyPath(c Change) bool {
	lower := strings.ToLower(c.File)
	for _, frag := range riskyPathFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// isTestPath reports whether a path looks like a test file: any directory
// segment is "test" or "tests" (case-insensitive), or the file name matches
// one of the conventions *_test.* (Go), test_* (Python), *.test.* / *.spec.*
// (JS/TS), or its stem ends in "Test" or "Tests" (case-sensitive; Java,
// Kotlin, C#).
func isTestPath(c Change) bool {
	segments := strings.Split(c.File, "/")
	base := segments[len(segments)-1]
	for _, dir := range segments[:len(segments)-1] {
		if lower := strings.ToLower(dir); lower == "test" || lower == "tests" {
			return true
		}
	}
	lowerBase := strings.ToLower(base)
	if strings.Contains(lowerBase, "_test.") || strings.HasPrefix(lowerBase, "test_") ||
		strings.Contains(lowerBase, ".test.") || strings.Contains(lowerBase, ".spec.") {
		return true
	}
	stem := base
	if i := strings.LastIndex(base, "."); i > 0 {
		stem = base[:i]
	}
	return strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests")
}

func isDocPath(c Change) bool {
	lower := strings.ToLower(c.File)
	if strings.HasSuffix(lower, ".md") {
		return true
	}
	segments := strings.Split(lower, "/")
	for _, dir := range segments[:len(segments)-1] {
		if dir == "docs" {
			return true
		}
	}
	return false
}

var configPathSuffixes = []string{".yaml", ".yml", ".json", ".toml", ".properties", ".gradle", ".kts"}

func isConfigPath(c Change) bool {
	lower := strings.ToLower(c.File)
	for _, suffix := range configPathSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func countHunkHeaders(c Change) int {
	n := 0
	for _, line := range strings.Split(c.Content, "\n") {
		if strings.HasPrefix(line, "@@") {
			n++
		}
	}
	return n
}

// NewDRSModelScorer loads the model artifact embedded in this binary
// (drs_model.json). It fails, and -scorer drs-model with it, while the
// committed artifact is the untrained placeholder.
func NewDRSModelScorer() (*DRSModelScorer, error) {
	s, err := newDRSModelScorerFromBytes(drsModelArtifactJSON)
	if err != nil {
		return nil, fmt.Errorf("radar: embedded drs_model.json: %w", err)
	}
	return s, nil
}

// newDRSModelScorerFromBytes decodes and validates a JSON model artifact.
func newDRSModelScorerFromBytes(data []byte) (*DRSModelScorer, error) {
	var artifact drsModelArtifact
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&artifact); err != nil {
		return nil, fmt.Errorf("decoding model artifact: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("model artifact must contain exactly one JSON object")
	}
	return newDRSModelScorer(artifact)
}

// newDRSModelScorer validates every field of the artifact up front so Score
// can never observe an unknown feature or transform, a degenerate scale, or a
// non-finite parameter.
func newDRSModelScorer(a drsModelArtifact) (*DRSModelScorer, error) {
	if a.Placeholder {
		return nil, errors.New("model artifact is a placeholder: no trained model has been committed")
	}
	if a.SchemaVersion != drsModelSchemaVersion {
		return nil, fmt.Errorf("unsupported model schema_version %d (want %d)", a.SchemaVersion, drsModelSchemaVersion)
	}
	if strings.TrimSpace(a.ModelVersion) == "" {
		return nil, errors.New("model_version is required")
	}
	if !finite(a.Intercept) {
		return nil, errors.New("intercept must be finite")
	}
	if len(a.Features) == 0 {
		return nil, errors.New("model artifact has no features")
	}
	s := &DRSModelScorer{version: a.ModelVersion, intercept: a.Intercept}
	seen := map[string]bool{}
	for _, f := range a.Features {
		feature, ok := drsModelFeatures[f.Name]
		if !ok {
			return nil, fmt.Errorf("unknown feature %q (known: %s)", f.Name, strings.Join(DRSModelFeatureNames(), ", "))
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("feature %q appears more than once", f.Name)
		}
		seen[f.Name] = true
		transform, ok := drsModelTransforms[f.Transform]
		if !ok {
			return nil, fmt.Errorf("feature %q: unknown transform %q", f.Name, f.Transform)
		}
		if !finite(f.Mean) || !finite(f.Scale) || !finite(f.Coefficient) {
			return nil, fmt.Errorf("feature %q: mean, scale, and coefficient must be finite", f.Name)
		}
		if f.Scale <= 0 {
			return nil, fmt.Errorf("feature %q: scale must be positive, got %v", f.Name, f.Scale)
		}
		s.terms = append(s.terms, drsModelTerm{
			feature: feature, transform: transform,
			mean: f.Mean, scale: f.Scale, coefficient: f.Coefficient,
		})
	}
	return s, nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// ModelVersion identifies the trained artifact, for audit output.
func (s *DRSModelScorer) ModelVersion() string { return s.version }

// Name returns the identifier used by the -scorer flag and
// PullRequestPolicy.CalibratedFor.
func (*DRSModelScorer) Name() string { return ScorerDRSModel }

// Score evaluates the linear model on the diff's features and returns the raw
// logit. Feature values are finite counts and every parameter was validated
// finite, so the result is finite in practice; should it ever overflow, the
// maximum float is returned so the diff calibrates as maximally risky rather
// than leaking a non-finite value.
func (s *DRSModelScorer) Score(d Diff) float64 {
	score := s.intercept
	for _, t := range s.terms {
		score += t.coefficient * ((t.transform(t.feature(d)) - t.mean) / t.scale)
	}
	if !finite(score) {
		return math.MaxFloat64
	}
	return score
}
