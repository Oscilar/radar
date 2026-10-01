package bedrock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/travisjeffery/radar"
)

const verdictJSON = `{"accept":true,"confidence":9,"risk_signals":[],"safe_signals":["doc-comment-update"],"reviewed_files":["README.md"],"findings":[],"summary":"typo fix"}`

func message(t *testing.T, stop string, content string) anthropic.Message {
	t.Helper()
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"us.anthropic.claude-sonnet-5","stop_reason":"` + stop + `",
		"usage":{"input_tokens":2255,"output_tokens":246,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},
		"content":[` + content + `]}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func toolUse(input string) string {
	return `{"type":"tool_use","id":"tu_1","name":"submit_verdict","input":` + input + `}`
}

func TestVerdictFrom(t *testing.T) {
	tests := []struct {
		name      string
		stop      string
		content   string
		wantErr   string
		truncated bool
	}{
		{name: "verdict", stop: "tool_use", content: `{"type":"thinking","thinking":"","signature":"s"},` + toolUse(verdictJSON)},
		{name: "truncated", stop: "max_tokens", content: `{"type":"thinking","thinking":"","signature":"s"}`, wantErr: "truncated", truncated: true},
		{name: "refusal", stop: "refusal", content: `{"type":"text","text":""}`, wantErr: "refused"},
		{name: "no tool call", stop: "end_turn", content: `{"type":"text","text":"looks fine"}`, wantErr: "expected one submit_verdict call, got 0"},
		{name: "two tool calls", stop: "tool_use", content: toolUse(verdictJSON) + "," + toolUse(verdictJSON), wantErr: "got 2"},
		{name: "verdict wrapped in verdict", stop: "tool_use", content: toolUse(`{"verdict":` + verdictJSON + `}`)},
		{name: "verdict wrapped in input", stop: "tool_use", content: toolUse(`{"input":` + verdictJSON + `}`)},
		{name: "wrapper beside other keys stays strict", stop: "tool_use", content: toolUse(`{"verdict":` + verdictJSON + `,"x":1}`), wantErr: "parsing verdict"},
		{name: "findings as a JSON string", stop: "tool_use", content: toolUse(`{"accept":true,"confidence":9,"risk_signals":[],"safe_signals":["doc-comment-update"],"reviewed_files":["README.md"],"findings":"[]","summary":"typo fix"}`)},
		{name: "summary that looks like an array stays a string", stop: "tool_use", content: toolUse(`{"accept":true,"confidence":9,"risk_signals":[],"safe_signals":["doc-comment-update"],"reviewed_files":["README.md"],"findings":[],"summary":"[x]"}`)},
		{name: "unknown field", stop: "tool_use", content: toolUse(`{"accept":true,"confidence":9,"risk_signals":[],"safe_signals":[],"reviewed_files":["a"],"findings":[],"summary":"s","extra":1}`), wantErr: "parsing verdict"},
		{name: "unknown signal", stop: "tool_use", content: toolUse(`{"accept":true,"confidence":9,"risk_signals":["vibes"],"safe_signals":[],"reviewed_files":["a"],"findings":[],"summary":"s"}`), wantErr: "unknown risk signal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := verdictFrom(message(t, tt.stop, tt.content))
			if got.Usage == nil || got.Usage.InputTokens != 2255 || got.Usage.OutputTokens != 246 || got.Usage.Requests != 1 {
				t.Fatalf("usage = %+v", got.Usage)
			}
			if got.Truncated != tt.truncated {
				t.Fatalf("truncated = %t, want %t", got.Truncated, tt.truncated)
			}
			if tt.wantErr == "" {
				if !got.Accept || got.Confidence != 9 || len(got.ReviewedFiles) != 1 {
					t.Fatalf("got %+v", got)
				}
				return
			}
			if got.Accept || got.Confidence != 0 || !strings.Contains(got.Summary, tt.wantErr) {
				t.Fatalf("must fail safe with %q, got %+v", tt.wantErr, got)
			}
		})
	}
}

func TestParamsAvoidFieldsBedrockRejects(t *testing.T) {
	a := &Agent{Model: "us.anthropic.claude-sonnet-5", MaxTokens: defaultMaxTokens, Effort: anthropic.OutputConfigEffortHigh}
	raw, err := json.Marshal(a.params(radar.Diff{Changes: []radar.Change{{File: "a", Content: "+x"}}}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	cfg, _ := body["output_config"].(map[string]any)
	if _, has := cfg["format"]; has {
		t.Fatal("Bedrock rejects output_config.format for Claude")
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if _, has := tool["strict"]; has {
		t.Fatal("Bedrock rejects strict tools for Claude")
	}
	if choice := body["tool_choice"].(map[string]any)["type"]; choice != "auto" {
		t.Fatalf("tool_choice = %v; forced choice is incompatible with thinking", choice)
	}
	if body["thinking"].(map[string]any)["type"] != "adaptive" {
		t.Fatalf("thinking = %v", body["thinking"])
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["additionalProperties"] != false || len(schema["required"].([]any)) != 7 {
		t.Fatalf("input schema = %v", schema)
	}
}
