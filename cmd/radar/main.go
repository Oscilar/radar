// Command radar is the CLI front-end to the RADAR funnel (arXiv:2605.30208).
//
// Usage:
//
//	radar classify     [-llm] [-scorer NAME] [-calibration sample.json] <diff.json>
//	radar replay       [-llm] [-scorer NAME] <diffs.json>
//	radar calibrate    [-scorer NAME] [-points N] <diffs.json>
//	radar drs-features [-names] <diffs.json>
//
// classify prints one diff's decision and trace; replay classifies a batch and
// prints RADAR RQ metrics; calibrate scores a batch and prints a quantile
// sample suitable for a policy's calibration_sample; drs-features prints every
// DRS model feature value per diff, as a golden for reimplementations.
//
// With -llm the Automated Code Review stage uses the Anthropic API (requires
// $ANTHROPIC_API_KEY); otherwise the deterministic rule-based ACR is used.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/travisjeffery/radar"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	switch cmd {
	case "classify":
		os.Exit(runClassify(os.Args[2:]))
	case "replay":
		os.Exit(runReplay(os.Args[2:]))
	case "calibrate":
		os.Exit(runCalibrate(os.Args[2:]))
	case "drs-features":
		os.Exit(runDRSFeatures(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "radar: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `radar — Risk Aware Diff Auto Review (arXiv:2605.30208)

Usage:
  radar classify     [-llm] [-json] [-scorer NAME] [-calibration sample.json] <diff.json>
  radar replay       [-llm] [-scorer NAME] <diffs.json>
  radar calibrate    [-scorer NAME] [-points N] <diffs.json>
  radar drs-features [-names] <diffs.json>

Flags:
  -llm          use the Anthropic-backed ACR agent (needs $ANTHROPIC_API_KEY)
  -json         (classify) emit the decision trace as JSON
  -scorer       risk scorer: heuristic (default) or drs-model
  -calibration  (classify) JSON array of raw scores to calibrate against, as
                printed by calibrate; required with -scorer drs-model
  -points       (calibrate) number of quantile points to print (default 41)
  -names        (drs-features) print only the sorted feature names
`)
}

func scorerFlag(fs *flag.FlagSet) *string {
	return fs.String("scorer", radar.ScorerHeuristic, "risk scorer: "+strings.Join(radar.ScorerNames(), " or "))
}

// newReviewAgent selects the ACR implementation.
func newReviewAgent(useLLM bool) (radar.ReviewAgent, error) {
	if !useLLM {
		return radar.RuleBasedAgent{}, nil
	}
	return radar.NewLLMAgent()
}

func runClassify(args []string) int {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	useLLM := fs.Bool("llm", false, "use the Anthropic-backed ACR agent")
	asJSON := fs.Bool("json", false, "emit the decision trace as JSON")
	scorerName := scorerFlag(fs)
	calibrationPath := fs.String("calibration", "", "JSON array of raw scores to calibrate against (required with -scorer drs-model)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "classify: expected exactly one <diff.json> argument")
		return 2
	}
	// DefaultCalibrationSample is on the heuristic scorer's scale; any other
	// scorer needs a sample computed with it (see radar calibrate). Unknown
	// names fall through to NewRiskScorer's error.
	if *calibrationPath == "" && *scorerName != radar.ScorerHeuristic && slices.Contains(radar.ScorerNames(), *scorerName) {
		fmt.Fprintf(os.Stderr, "classify: -scorer %s requires -calibration <sample.json> produced by `radar calibrate -scorer %s`\n", *scorerName, *scorerName)
		return 2
	}

	d, err := loadDiff(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		return 1
	}
	agent, err := newReviewAgent(*useLLM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		return 1
	}
	scorer, err := radar.NewRiskScorer(*scorerName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify:", err)
		return 1
	}
	sample := radar.DefaultCalibrationSample()
	if *calibrationPath != "" {
		if sample, err = loadCalibrationSample(*calibrationPath); err != nil {
			fmt.Fprintln(os.Stderr, "classify:", err)
			return 1
		}
	}

	eng := radar.NewEngine(
		radar.WithReviewAgent(agent),
		radar.WithScorer(scorer),
		radar.WithRunbooks(defaultRunbooks),
		radar.WithCalibrator(radar.NewCalibrator(sample)),
	)
	trace := eng.Classify(d)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(trace)
		return 0
	}
	printTrace(trace)
	return 0
}

func runReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	useLLM := fs.Bool("llm", false, "use the Anthropic-backed ACR agent")
	scorerName := scorerFlag(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "replay: expected exactly one <diffs.json> argument")
		return 2
	}

	diffs, err := loadDiffs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}
	agent, err := newReviewAgent(*useLLM)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}
	scorer, err := radar.NewRiskScorer(*scorerName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}

	// Calibrate DRS against the population of diffs being processed.
	eng := radar.NewEngine(
		radar.WithReviewAgent(agent),
		radar.WithScorer(scorer),
		radar.WithRunbooks(defaultRunbooks),
		radar.WithCalibrator(radar.NewCalibratorFromDiffs(scorer, diffs)),
	)

	var m metrics
	m.bySource = map[string]*sourceMetrics{}
	orgs := map[string]bool{}
	for _, d := range diffs {
		t := eng.Classify(d)
		m.record(d, t)
		orgs[d.Org] = true
	}
	m.orgs = len(orgs)
	m.print()
	return 0
}

// defaultCalibrationPoints is the sample size calibrate prints: 41 points
// gives 2.5-percentile resolution, enough for a P5/P20 gate to be meaningful
// while keeping a policy file readable.
const defaultCalibrationPoints = 41

// runCalibrate scores every diff in the input with the selected scorer and
// prints an ascending quantile sample for a policy's calibration_sample. It is
// the reproducible form of radar.NewCalibratorFromDiffs.
func runCalibrate(args []string) int {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	scorerName := scorerFlag(fs)
	points := fs.Int("points", defaultCalibrationPoints, "number of quantile points to print")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "calibrate: expected exactly one <diffs.json> argument")
		return 2
	}

	diffs, err := loadDiffs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		return 1
	}
	scorer, err := radar.NewRiskScorer(*scorerName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		return 1
	}
	scores := make([]float64, len(diffs))
	for i, d := range diffs {
		scores[i] = scorer.Score(d)
	}
	sort.Float64s(scores)
	sample, err := quantileSample(scores, *points)
	if err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		return 1
	}

	n := len(scores)
	fmt.Fprintf(os.Stderr, "calibrate: scorer=%s n=%d min=%g median=%g p90=%g max=%g; set \"calibrated_for\": %q alongside this sample\n",
		*scorerName, n, scores[0], scores[quantileIndex(0.5, n)], scores[quantileIndex(0.9, n)], scores[n-1], *scorerName)
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(sample); err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		return 1
	}
	return 0
}

// drsFeatureRow is one drs-features output record: a diff's id and every
// registered DRS model feature (encoding/json sorts the map keys).
type drsFeatureRow struct {
	ID       string             `json:"id"`
	Features map[string]float64 `json:"features"`
}

func drsFeatureRows(diffs []radar.Diff) []drsFeatureRow {
	rows := make([]drsFeatureRow, len(diffs))
	for i, d := range diffs {
		rows[i] = drsFeatureRow{ID: d.ID, Features: radar.DRSModelFeatureValues(d)}
	}
	return rows
}

// runDRSFeatures prints every DRS model feature value for every diff in the
// input (or, with -names, just the sorted feature names) as JSON on stdout. A
// reimplementation of the feature rules can diff its output against this.
func runDRSFeatures(args []string) int {
	fs := flag.NewFlagSet("drs-features", flag.ExitOnError)
	namesOnly := fs.Bool("names", false, "print only the sorted feature names")
	_ = fs.Parse(args)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *namesOnly {
		if fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "drs-features: -names takes no <diffs.json> argument")
			return 2
		}
		if err := enc.Encode(radar.DRSModelFeatureNames()); err != nil {
			fmt.Fprintln(os.Stderr, "drs-features:", err)
			return 1
		}
		return 0
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "drs-features: expected exactly one <diffs.json> argument")
		return 2
	}
	diffs, err := loadDiffs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "drs-features:", err)
		return 1
	}
	if err := enc.Encode(drsFeatureRows(diffs)); err != nil {
		fmt.Fprintln(os.Stderr, "drs-features:", err)
		return 1
	}
	return 0
}

// quantileSample returns points values from the ascending scores: for k in
// 0..points-1 the value at index round(k*(n-1)/(points-1)), so the first and
// last points are the min and max. Rounding is half away from zero
// (math.Round); a reimplementation must not use round-half-to-even.
func quantileSample(sorted []float64, points int) ([]float64, error) {
	n := len(sorted)
	if n == 0 {
		return nil, fmt.Errorf("no scores to sample")
	}
	if points < 2 {
		return nil, fmt.Errorf("points must be at least 2, got %d", points)
	}
	sample := make([]float64, points)
	for k := range sample {
		sample[k] = sorted[quantileIndex(float64(k)/float64(points-1), n)]
	}
	return sample, nil
}

// quantileIndex maps a quantile q in [0,1] to an index into n sorted values
// with the rounding rule quantileSample documents.
func quantileIndex(q float64, n int) int {
	return int(math.Round(q * float64(n-1)))
}

// loadCalibrationSample reads a JSON array of raw scores (calibrate's output).
func loadCalibrationSample(path string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sample []float64
	if err := json.Unmarshal(data, &sample); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(sample) == 0 {
		return nil, fmt.Errorf("%s: calibration sample is empty", path)
	}
	for _, v := range sample {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%s: calibration sample must be finite", path)
		}
	}
	return sample, nil
}

// --- metrics (RQ1/RQ2 definitions, Tables 3–5) ---

type sourceMetrics struct {
	reviewed int
	landed   int
}

type metrics struct {
	reviewed         int
	landed           int
	blanket          int
	autoLand         int
	verificationPass int
	radarApproved    int
	routeToHuman     int
	notEligible      int
	orgs             int
	bySource         map[string]*sourceMetrics
}

func (m *metrics) record(d radar.Diff, t *radar.DecisionTrace) {
	m.reviewed++
	sm := m.bySource[d.Source.String()]
	if sm == nil {
		sm = &sourceMetrics{}
		m.bySource[d.Source.String()] = sm
	}
	sm.reviewed++
	if t.Decision.Landed() {
		m.landed++
		sm.landed++
	}
	switch t.Decision {
	case radar.DecisionBlanketAutoAccept:
		m.blanket++
	case radar.DecisionAutoLand:
		m.autoLand++
	case radar.DecisionVerificationPassed:
		m.verificationPass++
	case radar.DecisionRADARApproved:
		m.radarApproved++
	case radar.DecisionRouteToHuman:
		m.routeToHuman++
	case radar.DecisionNotEligible:
		m.notEligible++
	}
}

func (m *metrics) print() {
	pct := func(n int) float64 {
		if m.reviewed == 0 {
			return 0
		}
		return float64(n) / float64(m.reviewed) * 100
	}
	approved := m.blanket + m.autoLand + m.radarApproved + m.verificationPass

	fmt.Println("RADAR replay — operational metrics")
	fmt.Println("===================================")
	fmt.Printf("RADAR reviewed diffs : %d\n", m.reviewed)
	fmt.Printf("RADAR landed diffs   : %d (%.1f%%)\n", m.landed, pct(m.landed))
	fmt.Printf("Coverage (orgs)      : %d\n", m.orgs)
	fmt.Println()
	fmt.Printf("Approve rate         : %.1f%% (%d eligible-approved)\n", pct(approved), approved)
	fmt.Printf("Verification pass    : %.1f%% (%d)\n", pct(m.verificationPass), m.verificationPass)
	fmt.Println()
	fmt.Println("Decision breakdown")
	fmt.Printf("  blanket-auto-accept: %d\n", m.blanket)
	fmt.Printf("  auto-land          : %d\n", m.autoLand)
	fmt.Printf("  verification-passed: %d\n", m.verificationPass)
	fmt.Printf("  radar-approved     : %d\n", m.radarApproved)
	fmt.Printf("  route-to-human     : %d\n", m.routeToHuman)
	fmt.Printf("  not-eligible       : %d\n", m.notEligible)
	fmt.Println()
	fmt.Println("By source type (reviewed / landed)")
	srcs := make([]string, 0, len(m.bySource))
	for s := range m.bySource {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		sm := m.bySource[s]
		fmt.Printf("  %-22s %d / %d\n", s, sm.reviewed, sm.landed)
	}
}

// --- input loading ---

func loadDiff(path string) (radar.Diff, error) {
	var d radar.Diff
	data, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("parsing %s: %w", path, err)
	}
	return d, nil
}

func loadDiffs(path string) ([]radar.Diff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var diffs []radar.Diff
	if err := json.Unmarshal(data, &diffs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return diffs, nil
}

func printTrace(t *radar.DecisionTrace) {
	fmt.Printf("diff %s\n", t.DiffID)
	fmt.Printf("decision: %s  (path: %s)\n", t.Decision, t.Path)
	if t.DRSPercentile >= 0 {
		fmt.Printf("DRS percentile: %.1f\n", t.DRSPercentile)
	}
	fmt.Println("stages:")
	for _, s := range t.Stages {
		mark := "✓"
		if !s.Passed {
			mark = "✗"
		}
		fmt.Printf("  %s %-26s %s\n", mark, s.Name, s.Reason)
	}
}
