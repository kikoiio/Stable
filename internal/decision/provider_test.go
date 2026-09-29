package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"proactive-agent/internal/appconfig"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const action = `{"kind":"ask_human","capability":"","target":"","parameters":{},"expected_artifact_id":"","reason":"needs review"}`

func TestProviderProtocols(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "gemini", "openai-compatible"} {
		t.Run(provider, func(t *testing.T) {
			p := &HTTPProvider{Config: appconfig.ModelConfig{Provider: provider, Model: "test-model", BaseURL: "http://127.0.0.1:9999/v1", APIKey: "secret-marker"}}
			p.Client = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "test-model") && provider != "gemini" {
					t.Fatal("model missing")
				}
				switch provider {
				case "openai":
					if r.Header.Get("Authorization") != "Bearer secret-marker" || r.URL.Path != "/v1/responses" {
						t.Fatal(r.URL, r.Header)
					}
				case "anthropic":
					if r.Header.Get("x-api-key") != "secret-marker" || r.Header.Get("anthropic-version") == "" {
						t.Fatal(r.Header)
					}
				case "gemini":
					if r.Header.Get("x-goog-api-key") != "secret-marker" || !strings.Contains(r.URL.Path, "test-model") {
						t.Fatal(r.URL, r.Header)
					}
				case "openai-compatible":
					if r.URL.Path != "/v1/chat/completions" {
						t.Fatal(r.URL)
					}
				}
				var response string
				switch provider {
				case "openai":
					response = `{"id":"resp1","status":"completed","output":[{"content":[{"type":"output_text","text":` + jsonString(action) + `}]}]}`
				case "anthropic":
					response = `{"id":"msg1","stop_reason":"end_turn","content":[{"type":"text","text":` + jsonString(action) + `}]}`
				case "gemini":
					response = `{"responseId":"gem1","candidates":[{"finishReason":"STOP","content":{"parts":[{"text":` + jsonString(action) + `}]}}]}`
				case "openai-compatible":
					response = `{"id":"chat1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + jsonString(action) + `}}]}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
			})}
			o, err := p.Generate(context.Background(), "choose", nil)
			if err != nil {
				t.Fatal(err)
			}
			if o.Proposal.Kind != "ask_human" || o.ProviderRequestID == "" {
				t.Fatal(o)
			}
		})
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestStrictActionAndSafeError(t *testing.T) {
	for _, v := range []string{`{"kind":"ask_human"}`, action + ` extra`, strings.Replace(action, `"kind":"ask_human"`, `"kind":"erase_all"`, 1), strings.Replace(action, `"reason":"needs review"`, `"reason":"needs review","extra":1`, 1)} {
		if _, err := parseAction([]byte(v)); err == nil {
			t.Fatalf("accepted %s", v)
		}
	}
	for _, code := range []int{401, 429, 500} {
		p := &HTTPProvider{Config: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:9999/v1", APIKey: "secret-marker"}}
		p.Client = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("secret-marker")), Header: http.Header{}}, nil
		})}
		_, err := p.Generate(context.Background(), "x", nil)
		if err == nil || strings.Contains(err.Error(), "secret-marker") {
			t.Fatalf("unsafe error %v", err)
		}
	}
}

func TestFailureMatrix(t *testing.T) {
	if _, err := NewProvider(appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:9999/v1"}); ErrorKind(err) != "missing_key" {
		t.Fatalf("missing key: %v", err)
	}
	respond := func(code int, body string) transport {
		return transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})
	}
	reply := func(content string) string {
		return `{"id":"x","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + jsonString(content) + `}}]}`
	}
	cases := []struct {
		name string
		rt   http.RoundTripper
		kind string
	}{
		{"timeout", transport(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded }), "timeout"},
		{"unauthorized", respond(401, "secret-marker"), "authentication"},
		{"rate_limited", respond(429, "slow down"), "rate_limited"},
		{"server_error", respond(500, "boom"), "provider_unavailable"},
		{"empty_body", respond(200, ""), "invalid_response"},
		{"no_choices", respond(200, `{"id":"x","choices":[]}`), "incomplete_response"},
		{"truncated_json", respond(200, reply(`{"kind":"execute_capabi`)), "invalid_json"},
		{"unknown_action", respond(200, reply(`{"kind":"erase_all","capability":"","target":"","parameters":{},"expected_artifact_id":"","reason":"x"}`)), "invalid_action"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &HTTPProvider{Config: appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: "http://127.0.0.1:9999/v1", APIKey: "secret-marker"}}
			p.Client = &http.Client{Transport: c.rt}
			_, err := p.Generate(context.Background(), "x", nil)
			if kind := ErrorKind(err); err == nil || kind != c.kind {
				t.Fatalf("kind %s want %s (%v)", kind, c.kind, err)
			}
			if strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
}
