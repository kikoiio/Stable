package decision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"strings"
	"testing"

	"stable/internal/appconfig"
)

// The repository schema file is the canonical definition; the wire schema
// built in Go must be structurally identical to it.
func TestCriteriaProposalSchemaFileMatches(t *testing.T) {
	path := filepath.Join("..", "..", "schemas", "criteria_proposal.schema.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	canonical := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var norm any
		if err = json.Unmarshal(b, &norm); err != nil {
			t.Fatal(err)
		}
		b, err = json.Marshal(norm)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if canonical(fromFileBytes(data)) != canonical(criteriaProposalSchema()) {
		t.Fatalf("schema drift between schemas/criteria_proposal.schema.json and criteriaProposalSchema()")
	}
}

func fromFileBytes(data []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m
}

func transpileProvider(t *testing.T, handler http.HandlerFunc) StructuredProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := appconfig.ModelConfig{Provider: "openai-compatible", Model: "mock", BaseURL: server.URL, APIKey: "test-key-123"}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p.(StructuredProvider)
}

func respond(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		msg, _ := json.Marshal(content)
		_, _ = w.Write([]byte(`{"id":"resp-1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` + string(msg) + `}}]}`))
	}
}

func TestTranspileAcceptsVocabularyMapping(t *testing.T) {
	var system string
	p := transpileProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			system = body.Messages[0].Content
		}
		respond(`{"status":"ok","criteria":[{"id":"erc-clean","kind":"kicad.erc_clean","payload":{"max_violations":0}},{"id":"j1","kind":"sensor.connection_present","payload":{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}}],"reason":"映射到 ERC 与连接检查"}`)(w, r)
	})
	res, err := Transpile(context.Background(), p, "ERC 必须全过，且 J1 连接要恢复")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "ok" || len(res.Criteria) != 2 {
		t.Fatalf("result: %+v", res)
	}
	if res.Criteria[0].Kind != "kicad.erc_clean" || res.Criteria[1].Kind != "sensor.connection_present" {
		t.Fatalf("criteria: %+v", res.Criteria)
	}
	if !strings.Contains(system, "status") || !strings.Contains(system, "criteria") {
		t.Fatalf("system prompt missing schema contract: %q", system)
	}
	if res.ProviderRequestID != "resp-1" {
		t.Fatalf("request id: %q", res.ProviderRequestID)
	}
}

func TestTranspileRejectsUnverifiableRequirements(t *testing.T) {
	p := transpileProvider(t, respond(`{"status":"reject","criteria":[],"reason":"美观无法机器验证"}`))
	res, err := Transpile(context.Background(), p, "让它看起来美观")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "reject" || res.Reason == "" {
		t.Fatalf("result: %+v", res)
	}
}

func TestTranspileRejectsOutOfVocabularyOutput(t *testing.T) {
	cases := []string{
		`{"status":"ok","criteria":[{"id":"beauty","kind":"kicad.beautiful","payload":{}}],"reason":""}`,
		`{"status":"ok","criteria":[{"id":"conn","kind":"sensor.connection_present","payload":{"endpoint_a":"R1.1","endpoint_b":"C3.2"}}],"reason":""}`,
		`{"status":"ok","criteria":[{"id":"erc","kind":"kicad.erc_clean","payload":{"max_violations":-2}}],"reason":""}`,
		`{"status":"ok","criteria":[],"reason":""}`,
		`{"status":"meh","criteria":[],"reason":"x"}`,
		`{"status":"reject","criteria":[],"reason":""}`,
		`{"status":"ok","criteria":[{"id":"erc","kind":"kicad.erc_clean","payload":{"max_violations":0},"extra":1}],"reason":""}`,
		`{"status":"ok","criteria":[{"id":"erc","kind":"kicad.erc_clean"}],"reason":""}`,
		`{"status":"ok","criteria":[{"id":"erc","kind":"kicad.erc_clean","payload":{"max_violations":0}}],"reason":"","extra":true}`,
	}
	for _, out := range cases {
		p := transpileProvider(t, respond(out))
		_, err := Transpile(context.Background(), p, "anything")
		if err == nil {
			t.Fatalf("accepted: %s", out)
		}
		if strings.Contains(err.Error(), "test-key-123") {
			t.Fatalf("leaked key: %v", err)
		}
	}
}

func TestTranspileTruncatedJSONFails(t *testing.T) {
	p := transpileProvider(t, respond(`{"status":"ok","criteria":[{"id":"er`))
	_, err := Transpile(context.Background(), p, "ERC")
	var api APIError
	if err == nil || !errors.As(err, &api) || api.Kind != "invalid_transpile" {
		t.Fatalf("expected invalid_transpile, got %v", err)
	}
}

func TestTranspileProviderErrorsPropagate(t *testing.T) {
	for _, code := range []int{401, 429, 500} {
		p := transpileProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		_, err := Transpile(context.Background(), p, "ERC")
		var api APIError
		if err == nil || !errors.As(err, &api) {
			t.Fatalf("code %d: expected APIError, got %v", code, err)
		}
		if strings.Contains(err.Error(), "test-key-123") {
			t.Fatalf("leaked key: %v", err)
		}
	}
}
