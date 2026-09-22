package radar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fireworksTestModel = "accounts/fireworks/models/glm-5p3"

func TestNewFireworksAgentFromEnvironment(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		model     string
		maxTokens string
		wantErr   string
	}{
		{name: "missing key", model: fireworksTestModel, wantErr: "FIREWORKS_API_KEY"},
		{name: "missing model", key: "test-key", wantErr: "RADAR_ACR_MODEL"},
		{name: "blank model", key: "test-key", model: "  ", wantErr: "RADAR_ACR_MODEL"},
		{name: "invalid max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "lots", wantErr: "RADAR_ACR_MAX_TOKENS"},
		{name: "zero max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "0", wantErr: "RADAR_ACR_MAX_TOKENS"},
		{name: "ok", key: "test-key", model: fireworksTestModel},
		{name: "ok with max tokens", key: "test-key", model: fireworksTestModel, maxTokens: "4096"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FIREWORKS_API_KEY", tt.key)
			t.Setenv("RADAR_ACR_MODEL", tt.model)
			t.Setenv("RADAR_ACR_MAX_TOKENS", tt.maxTokens)
			t.Setenv("FIREWORKS_BASE_URL", "")
			agent, err := NewFireworksAgent()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if agent.Model != fireworksTestModel || agent.BaseURL != fireworksChatCompletionsURL {
				t.Fatalf("unexpected agent: %+v", agent)
			}
			wantMax := defaultFireworksMaxTokens
			if tt.maxTokens != "" {
				wantMax = 4096
			}
			if agent.MaxTokens != wantMax {
				t.Fatalf("max tokens = %d, want %d", agent.MaxTokens, wantMax)
			}
			if agent.Describe() != "fireworks/"+fireworksTestModel {
				t.Fatalf("describe = %q", agent.Describe())
			}
		})
	}
}

func TestNewFireworksAgentHonoursBaseURLOverride(t *testing.T) {
	t.Setenv("FIREWORKS_API_KEY", "test-key")
	t.Setenv("RADAR_ACR_MODEL", fireworksTestModel)
	t.Setenv("FIREWORKS_BASE_URL", "http://127.0.0.1:9/v1/chat/completions")
	agent, err := NewFireworksAgent()
	if err != nil {
		t.Fatal(err)
	}
	if agent.BaseURL != "http://127.0.0.1:9/v1/chat/completions" {
		t.Fatalf("base URL = %q", agent.BaseURL)
	}
}

func TestFireworksAgentReviewUsesChatCompletions(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/inference/v1/chat/completions" {
			t.Errorf("path = %q, want /inference/v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"cmpl-1","model":"` + fireworksTestModel + `",
			"choices":[{"index":0,"finish_reason":"stop","message":{
				"role":"assistant",
				"reasoning_content":"docs only, nothing risky",
				"content":"{\"accept\":true,\"confidence\":10,\"risk_signals\":[],\"safe_signals\":[\"doc-comment-update\"],\"reviewed_files\":[\"README.md\"],\"findings\":[],\"summary\":\"documentation only\"}"
			}}],
			"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}
		}`))
	}))
	defer server.Close()

	agent := &FireworksAgent{
		APIKey:    "test-key",
		Model:     fireworksTestModel,
		BaseURL:   server.URL + "/inference/v1/chat/completions",
		MaxTokens: 4096,
		HTTP:      server.Client(),
	}
	result := agent.Review(Diff{
		ID:     "pr-123",
		Org:    "example",
		Source: SourceHuman,
		Changes: []Change{{
			File:       "README.md",
			Content:    "+clarify setup",
			Complexity: 1,
		}},
	})
	if !result.Accept || result.Confidence != 10 || result.Summary != "documentation only" {
		t.Fatalf("unexpected result: %+v", result)
	}

	if request["model"] != fireworksTestModel {
		t.Fatalf("model = %#v", request["model"])
	}
	if request["temperature"] != float64(0) {
		t.Fatalf("temperature = %#v, want 0", request["temperature"])
	}
	if request["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %#v, want 4096", request["max_tokens"])
	}
	messages := request["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want system + user", len(messages))
	}
	system := messages[0].(map[string]any)
	if system["role"] != "system" || !strings.Contains(system["content"].(string), "Respond with ONLY a JSON object") {
		t.Fatalf("system message = %#v", system)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" || !strings.Contains(user["content"].(string), "README.md") {
		t.Fatalf("user message = %#v", user)
	}

	format := request["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format type = %#v", format["type"])
	}
	jsonSchema := format["json_schema"].(map[string]any)
	if jsonSchema["name"] != "radar_acr_verdict" {
		t.Fatalf("json_schema name = %#v", jsonSchema["name"])
	}
	schema := jsonSchema["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatal("root schema must reject additional properties")
	}
	if properties := schema["properties"].(map[string]any); len(properties) != 7 {
		t.Fatalf("schema has %d properties, want 7", len(properties))
	}
}

func TestFireworksAgentStripsInlineThinkBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{
			"content":"<think>\nweighing the change\n</think>\n{\"accept\":false,\"confidence\":4,\"risk_signals\":[\"structural-change\"],\"safe_signals\":[],\"reviewed_files\":[\"src/a.go\"],\"findings\":[],\"summary\":\"reshapes the API\"}"
		}}]}`))
	}))
	defer server.Close()

	agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
	result := agent.Review(Diff{Changes: []Change{{File: "src/a.go", Content: "-x\n+y"}}})
	if result.Accept || result.Confidence != 4 || result.Summary != "reshapes the API" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result.RiskSignals) != 1 || result.RiskSignals[0] != SignalStructuralChange {
		t.Fatalf("risk signals = %v", result.RiskSignals)
	}
}

func TestStripThinkBlock(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"<think>hmm</think>{\"a\":1}", `{"a":1}`},
		{"  <think>\nmulti\nline\n</think>\n\n{\"a\":1}\n", `{"a":1}`},
		{"<think>never closed {\"a\":1}", "<think>never closed {\"a\":1}"},
		{"prefix <think>x</think>{\"a\":1}", "prefix <think>x</think>{\"a\":1}"},
	}
	for _, tt := range tests {
		if got := stripThinkBlock(tt.in); got != tt.want {
			t.Errorf("stripThinkBlock(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFireworksAgentFailsSafe(t *testing.T) {
	verdict := `{\"accept\":true,\"confidence\":10,\"risk_signals\":[],\"safe_signals\":[\"pure-formatting\"],\"reviewed_files\":[\"a.go\"],\"findings\":[],\"summary\":\"ok\"}`
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{
			name:       "API error status",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"message":"denied"}}`,
			want:       "fireworks API status 401",
		},
		{
			name:       "error object with 200",
			statusCode: http.StatusOK,
			body:       `{"error":{"message":"model overloaded"}}`,
			want:       "fireworks API error: model overloaded",
		},
		{
			name:       "no choices",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[]}`,
			want:       "no choices",
		},
		{
			name:       "truncated at max_tokens",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"length","message":{"content":"{\"accept\":tr"}}]}`,
			want:       "truncated at max_tokens",
		},
		{
			name:       "unexpected finish reason",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"content_filter","message":{"content":"` + verdict + `"}}]}`,
			want:       `finish_reason "content_filter"`,
		},
		{
			name:       "served a different model",
			statusCode: http.StatusOK,
			body:       `{"model":"accounts/fireworks/models/other","choices":[{"finish_reason":"stop","message":{"content":"` + verdict + `"}}]}`,
			want:       "served model",
		},
		{
			name:       "served model omitted",
			statusCode: http.StatusOK,
			body:       `{"choices":[{"finish_reason":"stop","message":{"content":"` + verdict + `"}}]}`,
			want:       `served model "", want`,
		},
		{
			name:       "finish reason omitted",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"message":{"content":"` + verdict + `"}}]}`,
			want:       `finish_reason ""`,
		},
		{
			name:       "refusal",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"","refusal":"cannot review"}}]}`,
			want:       "refused review",
		},
		{
			name:       "empty content",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"  "}}]}`,
			want:       "no content",
		},
		{
			name:       "malformed verdict",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"not json"}}]}`,
			want:       "invalid character",
		},
		{
			name:       "unknown signal",
			statusCode: http.StatusOK,
			body:       `{"model":"` + fireworksTestModel + `","choices":[{"finish_reason":"stop","message":{"content":"{\"accept\":true,\"confidence\":10,\"risk_signals\":[],\"safe_signals\":[\"looks-fine\"],\"reviewed_files\":[\"a.go\"],\"findings\":[],\"summary\":\"ok\"}"}}]}`,
			want:       `unknown safe signal "looks-fine"`,
		},
		{
			name:       "malformed response body",
			statusCode: http.StatusOK,
			body:       `{`,
			want:       "unexpected end of JSON input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			agent := &FireworksAgent{APIKey: "test-key", Model: fireworksTestModel, BaseURL: server.URL, HTTP: server.Client()}
			result := agent.Review(Diff{})
			if result.Accept || result.Confidence != 0 {
				t.Fatalf("error must fail safe: %+v", result)
			}
			if !strings.Contains(result.Summary, tt.want) {
				t.Fatalf("summary = %q, want substring %q", result.Summary, tt.want)
			}
		})
	}
}

func TestFireworksAgentFailsSafeWithoutModel(t *testing.T) {
	agent := &FireworksAgent{APIKey: "test-key", BaseURL: "http://127.0.0.1:9"}
	result := agent.Review(Diff{})
	if result.Accept || !strings.Contains(result.Summary, "model is required") {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestFireworksAgentFailsSafeOnTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{}`))
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	agent := &FireworksAgent{
		APIKey:  "test-key",
		Model:   fireworksTestModel,
		BaseURL: server.URL,
		HTTP:    &http.Client{Timeout: 50 * time.Millisecond},
	}
	result := agent.Review(Diff{})
	if result.Accept || result.Confidence != 0 || !strings.Contains(result.Summary, "failing safe") {
		t.Fatalf("timeout must fail safe: %+v", result)
	}
}
