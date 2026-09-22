package radar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// FireworksAgent is an Automated Code Review agent backed by an open-weight
// model served by Fireworks AI (https://fireworks.ai) through its
// OpenAI-compatible Chat Completions API. It reuses the shared ACR system
// prompt, verdict schema, and verdict parsing, so it is interchangeable with
// RuleBasedAgent, LLMAgent, and OpenAIAgent behind ReviewAgent, and it fails
// SAFE (non-accept) on any error.
//
// Unlike the OpenAI and Anthropic adapters there is no default model: Fireworks
// serves many interchangeable open-weight models and a silent default would
// let the reviewing model drift without a policy change. The full model id
// (for example accounts/fireworks/models/glm-5p3) is required, and the response
// must echo it back or the review fails safe.
type FireworksAgent struct {
	// APIKey authenticates to Fireworks. Defaults to $FIREWORKS_API_KEY.
	APIKey string
	// Model is the full Fireworks model id, e.g. accounts/fireworks/models/glm-5p3.
	// Read from $RADAR_ACR_MODEL; required.
	Model string
	// BaseURL is the Chat Completions endpoint. Defaults to
	// $FIREWORKS_BASE_URL or the public serverless endpoint.
	BaseURL string
	// MaxTokens caps the completion, including any reasoning tokens the model
	// spends before the verdict. Defaults to $RADAR_ACR_MAX_TOKENS or
	// defaultFireworksMaxTokens. A response cut off at this cap fails safe.
	MaxTokens int
	// HTTP is the client used for requests. Defaults to a 60s-timeout client.
	// Review additionally enforces a reviewTimeout deadline per call, so a
	// caller-supplied client without a Timeout cannot block the funnel forever.
	HTTP *http.Client
}

const (
	fireworksChatCompletionsURL = "https://api.fireworks.ai/inference/v1/chat/completions"
	// defaultFireworksMaxTokens leaves room for reasoning models, which spend
	// tokens thinking before the (small) JSON verdict. Fireworks' own default is
	// 2048, which a reasoning model can exhaust before it emits any verdict.
	defaultFireworksMaxTokens = 8192
)

// NewFireworksAgent constructs a FireworksAgent from the environment
// ($FIREWORKS_API_KEY, $RADAR_ACR_MODEL, optional $FIREWORKS_BASE_URL and
// $RADAR_ACR_MAX_TOKENS). It errors if the key or the model is missing.
func NewFireworksAgent() (*FireworksAgent, error) {
	key := os.Getenv("FIREWORKS_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("radar: FIREWORKS_API_KEY not set")
	}
	model := strings.TrimSpace(os.Getenv("RADAR_ACR_MODEL"))
	if model == "" {
		return nil, fmt.Errorf("radar: RADAR_ACR_MODEL is required for the fireworks agent (full id, e.g. accounts/fireworks/models/glm-5p3)")
	}
	baseURL := strings.TrimSpace(os.Getenv("FIREWORKS_BASE_URL"))
	if baseURL == "" {
		baseURL = fireworksChatCompletionsURL
	}
	maxTokens := defaultFireworksMaxTokens
	if raw := strings.TrimSpace(os.Getenv("RADAR_ACR_MAX_TOKENS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("radar: RADAR_ACR_MAX_TOKENS must be a positive integer, got %q", raw)
		}
		maxTokens = n
	}
	return &FireworksAgent{
		APIKey:    key,
		Model:     model,
		BaseURL:   baseURL,
		MaxTokens: maxTokens,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Describe names the provider and model for decision provenance.
func (a *FireworksAgent) Describe() string { return "fireworks/" + a.Model }

type fireworksChatReq struct {
	Model          string                  `json:"model"`
	Messages       []fireworksChatMessage  `json:"messages"`
	Temperature    float64                 `json:"temperature"`
	MaxTokens      int                     `json:"max_tokens"`
	ResponseFormat fireworksResponseFormat `json:"response_format"`
}

type fireworksChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type fireworksResponseFormat struct {
	Type       string              `json:"type"`
	JSONSchema fireworksJSONSchema `json:"json_schema"`
}

type fireworksJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
}

type fireworksChatResp struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Review sends the diff to the Fireworks-served model and returns the parsed
// verdict, failing safe (non-accept) on any error.
func (a *FireworksAgent) Review(d Diff) ACRResult {
	ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
	defer cancel()
	res, err := a.review(ctx, d)
	if err != nil {
		return ACRResult{Accept: false, Confidence: 0, Summary: "ACR Fireworks error, failing safe: " + err.Error()}
	}
	return res
}

func (a *FireworksAgent) review(ctx context.Context, d Diff) (ACRResult, error) {
	if strings.TrimSpace(a.Model) == "" {
		return ACRResult{}, fmt.Errorf("fireworks model is required")
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	url := a.BaseURL
	if url == "" {
		url = fireworksChatCompletionsURL
	}
	maxTokens := a.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultFireworksMaxTokens
	}

	reqBody := fireworksChatReq{
		Model: a.Model,
		Messages: []fireworksChatMessage{
			{Role: "system", Content: acrSystemPrompt},
			{Role: "user", Content: renderDiffForReview(d)},
		},
		Temperature: 0,
		MaxTokens:   maxTokens,
		ResponseFormat: fireworksResponseFormat{
			Type:       "json_schema",
			JSONSchema: fireworksJSONSchema{Name: "radar_acr_verdict", Schema: acrVerdictSchema()},
		},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return ACRResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return ACRResult{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+a.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return ACRResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ACRResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ACRResult{}, fmt.Errorf("fireworks API status %d: %s", resp.StatusCode, string(body))
	}

	var fr fireworksChatResp
	if err := json.Unmarshal(body, &fr); err != nil {
		return ACRResult{}, err
	}
	if fr.Error != nil {
		return ACRResult{}, fmt.Errorf("fireworks API error: %s", fr.Error.Message)
	}
	// A response that does not name the model that served it cannot prove the
	// configured model produced the verdict, so it fails safe like a mismatch.
	if fr.Model != a.Model {
		return ACRResult{}, fmt.Errorf("fireworks served model %q, want %q", fr.Model, a.Model)
	}
	if len(fr.Choices) == 0 {
		return ACRResult{}, fmt.Errorf("fireworks API returned no choices")
	}
	choice := fr.Choices[0]
	switch choice.FinishReason {
	case "stop":
	case "length":
		return ACRResult{}, fmt.Errorf("fireworks response truncated at max_tokens=%d", maxTokens)
	default:
		// Includes a missing finish_reason: without an explicit stop the
		// completion cannot be shown to be complete.
		return ACRResult{}, fmt.Errorf("fireworks finish_reason %q", choice.FinishReason)
	}
	text := stripThinkBlock(choice.Message.Content)
	if strings.TrimSpace(text) == "" {
		if choice.Message.Refusal != "" {
			return ACRResult{}, fmt.Errorf("fireworks API refused review")
		}
		return ACRResult{}, fmt.Errorf("fireworks API returned no content")
	}
	return parseACRVerdict(text)
}

// stripThinkBlock removes a leading <think>…</think> block. Fireworks normally
// returns reasoning in a separate reasoning_content field, but some open-weight
// models inline it in content; the verdict parser rejects anything but a
// single JSON object, so the block would otherwise fail every review safe.
func stripThinkBlock(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "<think>") {
		return content
	}
	end := strings.Index(trimmed, "</think>")
	if end < 0 {
		return content
	}
	return strings.TrimSpace(trimmed[end+len("</think>"):])
}
