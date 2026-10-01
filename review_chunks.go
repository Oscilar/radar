package radar

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// defaultReviewChunkChars keeps one request well inside a 128k-token context
// (roughly four characters per token) with room for the prompt and verdict.
const defaultReviewChunkChars = 240_000

// maxConcurrentChunks bounds parallel requests per reviewer so a large diff
// fits the job deadline without bursting the provider's rate limit.
const maxConcurrentChunks = 3

// chunkOverheadChars approximates the per-change header renderDiffForReview adds.
const chunkOverheadChars = 64

// chunkDiff splits d into parts whose patch text fits budget, keeping each
// file whole. A single file over budget cannot be reviewed and is an error,
// so it fails safe instead of being truncated.
func chunkDiff(d Diff, budget int) ([]Diff, error) {
	var parts [][]Change
	size := 0
	for _, c := range d.Changes {
		n := len(c.Content) + len(c.File) + chunkOverheadChars
		if n > budget {
			return nil, fmt.Errorf("patch for %s is %d characters, over the %d-character review budget", c.File, n, budget)
		}
		if len(parts) == 0 || size+n > budget {
			parts = append(parts, nil)
			size = 0
		}
		parts[len(parts)-1] = append(parts[len(parts)-1], c)
		size += n
	}
	if len(parts) <= 1 {
		return []Diff{d}, nil
	}
	out := make([]Diff, len(parts))
	for i, changes := range parts {
		part := d
		part.Changes = changes
		part.Part, part.Parts = i+1, len(parts)
		out[i] = part
	}
	return out, nil
}

// reviewInChunks runs agent over d, splitting it when it exceeds budget, and
// merges the parts into one verdict.
func reviewInChunks(agent ReviewAgent, d Diff, budget int) ACRResult {
	parts, err := chunkDiff(d, budget)
	if err != nil {
		return ACRResult{Summary: "diff not reviewed, failing safe: " + err.Error()}
	}
	if len(parts) == 1 {
		return agent.Review(parts[0])
	}
	start := time.Now()
	results := make([]ACRResult, len(parts))
	sem := make(chan struct{}, maxConcurrentChunks)
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = agent.Review(part)
		}()
	}
	wg.Wait()
	merged := mergeVerdicts(results, func(i int) string { return fmt.Sprintf("part %d/%d", i+1, len(results)) })
	elapsed := time.Since(start).Milliseconds()
	merged.ElapsedMS = &elapsed
	return merged
}

// mergeVerdicts combines verdicts conservatively: every part must accept, the
// lowest confidence wins, and signals, findings and reviewed files accumulate.
func mergeVerdicts(results []ACRResult, label func(int) string) ACRResult {
	merged := ACRResult{Accept: len(results) > 0, ModelAccept: len(results) > 0, Confidence: ACRMaxConfidence}
	seenRisk := map[ChangeSignal]bool{}
	seenSafe := map[ChangeSignal]bool{}
	summaries := make([]string, 0, len(results))
	for i, res := range results {
		merged.Accept = merged.Accept && res.Accept
		merged.ModelAccept = merged.ModelAccept && (res.ModelAccept || res.Accept)
		merged.Confidence = min(merged.Confidence, res.Confidence)
		for _, s := range res.RiskSignals {
			if !seenRisk[s] {
				seenRisk[s] = true
				merged.RiskSignals = append(merged.RiskSignals, s)
			}
		}
		for _, s := range res.SafeSignals {
			if !seenSafe[s] {
				seenSafe[s] = true
				merged.SafeSignals = append(merged.SafeSignals, s)
			}
		}
		merged.ReviewedFiles = append(merged.ReviewedFiles, res.ReviewedFiles...)
		merged.Findings = append(merged.Findings, res.Findings...)
		summaries = append(summaries, label(i)+": "+res.Summary)
	}
	if len(results) == 0 {
		merged.Confidence = 0
	}
	merged.Summary = strings.Join(summaries, " | ")
	return merged
}
