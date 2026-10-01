// Package bedrock is a Radar review agent backed by Claude on Amazon Bedrock.
// It lives outside the radar package so the core library keeps no third-party
// dependencies.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/travisjeffery/radar"
)

const (
	// Reasoning shares the output budget with the verdict; Sonnet 5 allows
	// 128K, and a review that needs more than this is split by Radar.
	defaultMaxTokens = 64000
	defaultTimeout   = 480 * time.Second
	verdictTool      = "submit_verdict"
)

// Bedrock rejects output_config.format and strict tools for Claude, so the
// verdict arrives as the input of a plain tool and ParseACRVerdict enforces
// the schema.
const toolInstruction = "\n\nSubmit your verdict by calling the " + verdictTool + " tool exactly once, passing the verdict's fields (accept, confidence, risk_signals, safe_signals, reviewed_files, findings, summary) directly as the tool's arguments, not nested under another key. Write nothing else."

// Agent reviews diffs with a Claude model through Bedrock InvokeModel. It
// fails safe: any error, refusal or missing verdict routes to a human.
type Agent struct {
	// Model is a Bedrock model or inference profile id, such as
	// us.anthropic.claude-sonnet-5.
	Model     string
	MaxTokens int64
	Effort    anthropic.OutputConfigEffort
	Timeout   time.Duration
	client    anthropic.Client
}

// NewAgent builds an agent from the AWS default credential chain and region
// ($AWS_REGION) plus optional $RADAR_BEDROCK_MAX_TOKENS,
// $RADAR_BEDROCK_EFFORT and $RADAR_ACR_TIMEOUT.
func NewAgent(ctx context.Context, model string) (*Agent, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("radar: the bedrock agent needs a model or inference profile id, e.g. us.anthropic.claude-sonnet-5")
	}
	if os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
		return nil, fmt.Errorf("radar: AWS_REGION is required for the bedrock agent")
	}
	a := &Agent{Model: model, MaxTokens: defaultMaxTokens, Effort: anthropic.OutputConfigEffortHigh, Timeout: defaultTimeout}
	if raw := strings.TrimSpace(os.Getenv("RADAR_BEDROCK_MAX_TOKENS")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("radar: RADAR_BEDROCK_MAX_TOKENS must be a positive integer, got %q", raw)
		}
		a.MaxTokens = n
	}
	if raw := strings.TrimSpace(os.Getenv("RADAR_BEDROCK_EFFORT")); raw != "" {
		a.Effort = anthropic.OutputConfigEffort(raw)
	}
	if raw := strings.TrimSpace(os.Getenv("RADAR_ACR_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("radar: RADAR_ACR_TIMEOUT must be a positive duration such as 480s, got %q", raw)
		}
		a.Timeout = d
	}
	a.client = anthropic.NewClient(bedrock.WithLoadDefaultConfig(ctx))
	return a, nil
}

// Describe names the provider and model for decision provenance.
func (a *Agent) Describe() string { return "bedrock/" + a.Model }

// Review sends the diff and returns the parsed verdict.
func (a *Agent) Review(d radar.Diff) radar.ACRResult {
	ctx, cancel := context.WithTimeout(context.Background(), a.Timeout)
	defer cancel()
	start := time.Now()
	res := a.review(ctx, d)
	elapsed := time.Since(start).Milliseconds()
	res.ElapsedMS = &elapsed
	return res
}

func (a *Agent) review(ctx context.Context, d radar.Diff, opts ...option.RequestOption) radar.ACRResult {
	stream := a.client.Messages.NewStreaming(ctx, a.params(d), opts...)
	var msg anthropic.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return failSafe(fmt.Errorf("reading stream: %w", err), nil)
		}
	}
	if err := stream.Err(); err != nil {
		return failSafe(err, nil)
	}
	return verdictFrom(msg)
}

func (a *Agent) params(d radar.Diff) anthropic.MessageNewParams {
	schema := radar.ACRVerdictSchema()
	return anthropic.MessageNewParams{
		Model:     anthropic.Model(a.Model),
		MaxTokens: a.MaxTokens,
		System:    []anthropic.TextBlockParam{{Text: radar.ACRSystemPrompt + toolInstruction}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(radar.RenderDiffForReview(d)))},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: a.Effort,
		},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        verdictTool,
			Description: anthropic.String("Record the review verdict for this diff."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties:  schema["properties"],
				Required:    requiredOf(schema),
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}}},
		// Forced tool choice is incompatible with thinking.
		ToolChoice: anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}},
	}
}

func requiredOf(schema map[string]any) []string {
	required, _ := schema["required"].([]string)
	return required
}

// verdictFrom turns a finished response into a verdict.
func verdictFrom(msg anthropic.Message) radar.ACRResult {
	usage := &radar.ReviewUsage{
		InputTokens:      msg.Usage.InputTokens,
		OutputTokens:     msg.Usage.OutputTokens,
		CacheReadTokens:  msg.Usage.CacheReadInputTokens,
		CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		Requests:         1,
	}
	switch msg.StopReason {
	case anthropic.StopReasonMaxTokens:
		res := failSafe(fmt.Errorf("response truncated at max_tokens"), usage)
		res.Truncated = true
		return res
	case anthropic.StopReasonRefusal:
		return failSafe(fmt.Errorf("model refused the review"), usage)
	}
	var calls []json.RawMessage
	for _, block := range msg.Content {
		if block.Type == "tool_use" && block.Name == verdictTool {
			calls = append(calls, block.Input)
		}
	}
	if len(calls) != 1 {
		return failSafe(fmt.Errorf("expected one %s call, got %d (stop %s)", verdictTool, len(calls), msg.StopReason), usage)
	}
	res, err := radar.ParseACRVerdict(string(unwrapVerdict(calls[0])))
	if err != nil {
		return failSafe(fmt.Errorf("parsing verdict: %w", err), usage)
	}
	res.Usage = usage
	return res
}

// unwrapVerdict undoes two shapes the model sometimes produces without a
// strict schema: the verdict nested under a single "verdict" or "input" key,
// and an array field sent as a JSON-encoded string. The result is still
// validated strictly.
func unwrapVerdict(input json.RawMessage) json.RawMessage {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(input, &outer); err != nil {
		return input
	}
	if len(outer) == 1 {
		for _, key := range []string{"verdict", "input"} {
			if inner, ok := outer[key]; ok && len(inner) > 0 && inner[0] == '{' {
				return unwrapVerdict(inner)
			}
		}
	}
	changed := false
	for key, value := range outer {
		var text string
		if json.Unmarshal(value, &text) != nil {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "[") && json.Valid([]byte(trimmed)) {
			outer[key] = json.RawMessage(trimmed)
			changed = true
		}
	}
	if !changed {
		return input
	}
	out, err := json.Marshal(outer)
	if err != nil {
		return input
	}
	return out
}

func failSafe(err error, usage *radar.ReviewUsage) radar.ACRResult {
	return radar.ACRResult{Summary: "ACR Bedrock error, failing safe: " + err.Error(), Usage: usage}
}
