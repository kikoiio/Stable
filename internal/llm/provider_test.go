package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"stable/internal/appconfig"
)

func TestNewProviderFactory(t *testing.T) {
	tests := []struct {
		name string
		cfg  appconfig.ModelConfig
		want string
	}{
		{"anthropic", appconfig.ModelConfig{Provider: "anthropic", Model: "claude", APIKey: "test"}, "llm.anthropicProvider"},
		{"openai", appconfig.ModelConfig{Provider: "openai", Model: "gpt", APIKey: "test"}, "llm.openAIProvider"},
		{"compatible", appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:1234/v1", APIKey: "test"}, "llm.compatibleProvider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := NewProvider(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%T", provider); got != tt.want {
				t.Fatalf("type=%s want %s", got, tt.want)
			}
		})
	}
	if _, err := NewProvider(appconfig.ModelConfig{Provider: "gemini", Model: "gemini", APIKey: "test"}); err == nil {
		t.Fatal("Gemini streaming unexpectedly supported")
	}
	if _, err := NewProvider(appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://example.com/v1", APIKey: "test"}); err == nil {
		t.Fatal("non-loopback HTTP endpoint accepted")
	}
}

func TestAnthropicStreamEventsAndUsage(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":3}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"read\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":4}}\n\n"
	client := fixtureClient(t, "https://api.anthropic.com/v1/messages", "x-api-key", "test-secret", body)
	provider := anthropicProvider{config: Config{Model: appconfig.ModelConfig{Provider: "anthropic", Model: "claude-test", APIKey: "test-secret"}, HTTPClient: client}}
	events, err := collect(t, provider, Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []EventKind{TextDelta, ToolCallStart, ToolCallDelta, ToolCallComplete, Usage, StreamEnd}
	if got := eventKinds(events); !equalKinds(got, want) {
		t.Fatalf("event kinds=%v want %v", got, want)
	}
	if events[3].Tool == nil || events[3].Tool.ID != "call-1" || !events[3].Tool.Complete {
		t.Fatalf("tool event=%+v", events[3].Tool)
	}
	if events[4].Usage.InputTokens == nil || *events[4].Usage.InputTokens != 12 || events[4].Usage.CacheReadTokens == nil || *events[4].Usage.CacheReadTokens != 3 {
		t.Fatalf("usage=%+v", events[4].Usage)
	}
}

func TestOpenAIResponsesStreamAndUsage(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.done\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":2}}}}\n\n"
	client := fixtureClient(t, "https://api.openai.com/v1/responses", "Authorization", "Bearer key", body)
	cfg := Config{Model: appconfig.ModelConfig{Provider: "openai", Model: "gpt-test", APIKey: "key"}, HTTPClient: client}
	events, err := collect(t, openAIProvider{config: cfg}, Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []EventKind{TextDelta, ThinkingDelta, ThinkingComplete, Usage, StreamEnd}
	if got := eventKinds(events); !equalKinds(got, want) {
		t.Fatalf("event kinds=%v want %v", got, want)
	}
	if events[3].Usage.InputTokens == nil || *events[3].Usage.InputTokens != 10 || events[3].Usage.CacheReadTokens == nil || *events[3].Usage.CacheReadTokens != 2 {
		t.Fatalf("usage=%+v", events[3].Usage)
	}
}

func TestCompatibleStreamToolCallsAndMissingUsage(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"list\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	client := fixtureClient(t, "http://127.0.0.1:1234/v1/chat/completions", "Authorization", "Bearer key", body)
	cfg := Config{Model: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:1234/v1", APIKey: "key"}, HTTPClient: client}
	events, err := collect(t, compatibleProvider{config: cfg}, Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []EventKind{TextDelta, ToolCallStart, ToolCallDelta, ToolCallComplete, StreamEnd}
	if got := eventKinds(events); !equalKinds(got, want) {
		t.Fatalf("event kinds=%v want %v", got, want)
	}
	if events[len(events)-1].Usage != nil {
		t.Fatalf("missing usage became known: %+v", events[len(events)-1].Usage)
	}
}

func TestProviderErrorClassAndRetryAfter(t *testing.T) {
	err := classifyHTTPError(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"2"}}, []byte(`{"error":{"type":"rate_limit"}}`))
	if err.Class != ErrorRateLimit || !err.Retryable || err.RetryAfter != 2*time.Second {
		t.Fatalf("error=%+v", err)
	}
	ctxErr := classifyHTTPError(http.StatusBadRequest, nil, []byte(`{"error":{"code":"context_length_exceeded"}}`))
	if ctxErr.Class != ErrorContextLimit || ctxErr.Retryable {
		t.Fatalf("error=%+v", ctxErr)
	}
	secret := "test-secret"
	auth := classifyHTTPError(http.StatusUnauthorized, nil, []byte(`{"error":{"message":"test-secret leaked"}}`))
	if auth.Class != ErrorAuth || strings.Contains(auth.Error(), secret) {
		t.Fatalf("unsafe error: %v", auth)
	}
}

func TestTruncatedStreamIsRetryableNetworkAndMalformedJSONIsProviderError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       ErrorClass
		retry      bool
	}{
		{"truncated", "data: {\"choices\":", ErrorNetwork, true},
		{"malformed", "data: {not-json}\n\n", ErrorProvider, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fixtureClient(t, "http://127.0.0.1:1234/v1/chat/completions", "Authorization", "Bearer key", tc.body)
			provider := compatibleProvider{config: Config{Model: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:1234/v1", APIKey: "key"}, HTTPClient: client}}
			_, err := collect(t, provider, Request{Messages: []Message{{Role: "user", Content: "hello"}}})
			pe, ok := err.(*ProviderError)
			if !ok || pe.Class != tc.want || pe.Retryable != tc.retry {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestStreamCancelClosesHTTPRequest(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		body := &blockingBody{ctx: req.Context(), closed: closed}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{Model: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:1234/v1", APIKey: "key"}, HTTPClient: client}
	events, errs := compatibleProvider{config: cfg}.Stream(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("response body was not closed after cancellation")
	}
	for range events {
	}
	for range errs {
	}
}

func collect(t *testing.T, provider Provider, req Request) ([]Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	events, errs := provider.Stream(ctx, req)
	var got []Event
	for event := range events {
		got = append(got, event)
	}
	var result error
	for err := range errs {
		if err != nil {
			result = err
		}
	}
	return got, result
}

func eventKinds(events []Event) []EventKind {
	out := make([]EventKind, len(events))
	for i := range events {
		out[i] = events[i].Kind
	}
	return out
}

func equalKinds(a, b []EventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fixtureClient(t *testing.T, wantURL, header, wantHeader, sse string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.String(); got != wantURL {
			t.Errorf("URL=%s want %s", got, wantURL)
		}
		if got := req.Header.Get(header); got != wantHeader {
			t.Errorf("%s=%s want %s", header, got, wantHeader)
		}
		if req.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept=%q", req.Header.Get("Accept"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse)), Request: req}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type blockingBody struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *blockingBody) Close() error             { b.once.Do(func() { close(b.closed) }); return nil }

var _ io.ReadCloser = (*blockingBody)(nil)
