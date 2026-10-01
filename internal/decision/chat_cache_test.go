package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"stable/internal/appconfig"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicCachesStableSystemAndCompatibleProviderDoesNot(t *testing.T) {
	messages := []ChatMessage{{Role: "system", Content: "stable", Cache: true}, {Role: "system", Content: "dynamic goal context"}, {Role: "user", Content: "dynamic"}}
	var anthropic map[string]any
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&anthropic); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`))}, nil
	})}
	p := &HTTPProvider{Config: appconfig.ModelConfig{Provider: "anthropic", Model: "test", APIKey: "secret"}, Client: client}
	if answer, err := p.GenerateChat(context.Background(), messages); err != nil || answer != "ok" {
		t.Fatalf("response %q %v", answer, err)
	}
	system, ok := anthropic["system"].([]any)
	if !ok || len(system) != 2 || system[0].(map[string]any)["cache_control"] == nil || system[1].(map[string]any)["cache_control"] != nil || system[1].(map[string]any)["text"] != "dynamic goal context" {
		t.Fatalf("Anthropic cache prefix: %#v", anthropic["system"])
	}
	var compatible map[string]any
	client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&compatible); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))}, nil
	})}
	p = &HTTPProvider{Config: appconfig.ModelConfig{Provider: "openai-compatible", Model: "test", BaseURL: "https://mock.invalid", APIKey: "secret"}, Client: client}
	if _, err := p.GenerateChat(context.Background(), messages); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(compatible)
	if strings.Contains(string(b), "cache_control") || strings.Contains(string(b), "Cache") {
		t.Fatalf("unsupported provider got cache fields: %s", b)
	}
}
