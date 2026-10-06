package hooks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunHTTPActionMethodHeadersAndBody(t *testing.T) {
	t.Setenv("HOOKS_TEST_TOKEN", "s3cret")
	var method, uri, token, contentType, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, uri = r.Method, r.URL.RequestURI()
		token = r.Header.Get("X-Stable-Token")
		contentType = r.Header.Get("Content-Type")
		body = string(b)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	out, err := runHTTPAction(Action{
		URL:     srv.URL + "/hook?run=$HOOKS_TEST_TOKEN",
		Headers: map[string]string{"X-Stable-Token": "$HOOKS_TEST_TOKEN"},
	}, 5*time.Second)
	if err != nil || out != "ok" {
		t.Fatalf("GET: out=%q err=%v", out, err)
	}
	if method != http.MethodGet || uri != "/hook?run=s3cret" || token != "s3cret" {
		t.Fatalf("GET request mismatch: method=%s uri=%q token=%q", method, uri, token)
	}

	out, err = runHTTPAction(Action{
		URL:     srv.URL,
		Method:  "post",
		Headers: map[string]string{"X-Stable-Token": "$HOOKS_TEST_TOKEN", "Content-Type": "application/json"},
		Body:    `{"k":"$HOOKS_TEST_TOKEN"}`,
	}, 5*time.Second)
	if err != nil || out != "ok" {
		t.Fatalf("POST: out=%q err=%v", out, err)
	}
	if method != http.MethodPost || token != "s3cret" || contentType != "application/json" {
		t.Fatalf("POST request mismatch: method=%s token=%q content-type=%q", method, token, contentType)
	}
	// Body is sent verbatim: no env expansion inside the body.
	if body != `{"k":"$HOOKS_TEST_TOKEN"}` {
		t.Fatalf("body not verbatim: %q", body)
	}
}

func TestRunHTTPActionDefaultMethodAndNoContentType(t *testing.T) {
	var method, contentType, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, contentType, body = r.Method, r.Header.Get("Content-Type"), string(b)
		_, _ = w.Write([]byte("done"))
	}))
	defer srv.Close()

	out, err := runHTTPAction(Action{URL: srv.URL, Body: "raw payload"}, 5*time.Second)
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if method != http.MethodGet {
		t.Fatalf("method default: got %s, want GET", method)
	}
	// Content-Type must not be invented when Headers leave it out.
	if contentType != "" {
		t.Fatalf("Content-Type invented: %q", contentType)
	}
	if body != "raw payload" {
		t.Fatalf("body mismatch: %q", body)
	}
}

func TestRunHTTPActionTwoXXBodyBecomesOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello\nhook\n"))
	}))
	defer srv.Close()

	out, err := runHTTPAction(Action{URL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Surrounding whitespace trimmed, like command output handling.
	if out != "hello\nhook" {
		t.Fatalf("out=%q", out)
	}
}

func TestRunHTTPActionLargeBodyPassesThrough(t *testing.T) {
	// 20 KB exceeds the hook journal limit (sessionlog.MaxHookOutput, 8 KiB)
	// but runHTTPAction must not truncate it: hook output truncation happens
	// in the caller (conversation.HookGate.runOne), exactly as for command
	// actions.
	payload := strings.Repeat("x", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	out, err := runHTTPAction(Action{URL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != len(payload) || !strings.HasPrefix(out, "xxxx") {
		t.Fatalf("large body altered: len=%d", len(out))
	}
}

func TestRunHTTPActionNon2xxIncludesStatusAndDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.Error(w, "missing thing", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("y", 1000)))
		}
	}))
	defer srv.Close()

	_, err := runHTTPAction(Action{URL: srv.URL + "/missing"}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "missing thing") {
		t.Fatalf("expected status and body digest in error, got %v", err)
	}

	_, err = runHTTPAction(Action{URL: srv.URL + "/huge", Method: http.MethodGet}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 in error, got %v", err)
	}
	// Digest is capped at 512 bytes of body.
	if !strings.Contains(err.Error(), strings.Repeat("y", 512)) || strings.Contains(err.Error(), strings.Repeat("y", 513)) {
		t.Fatalf("digest not capped at 512 bytes: %v", err)
	}
}

func TestRunHTTPActionTimeoutDistinguishable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	defer slow.Close()

	_, err := runHTTPAction(Action{URL: slow.URL}, 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout-flavored error, got %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	_, err = runHTTPAction(Action{URL: deadURL}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "failed") || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected generic failure without timeout flavor, got %v", err)
	}
}

func TestFireHTTPActionOnErrorSemantics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("fine"))
			return
		}
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()

	// Success path: 2xx body becomes output, hook not rejected.
	got := FireOne(Hook{ID: "ping-ok", Action: Action{Type: "http", URL: srv.URL + "/ok"}}, Context{})
	if !got.Success || got.Rejected {
		t.Fatalf("unexpected success result: %+v", got)
	}

	// pre_tool_use + on_error reject: failing hook is a rejection.
	got = FireOne(Hook{ID: "guard", Event: EventPreToolUse, Action: Action{Type: "http", URL: srv.URL}, OnError: "reject"}, Context{Event: EventPreToolUse})
	if got.Success || !got.Rejected || !strings.Contains(got.Output, "403") {
		t.Fatalf("unexpected reject result: %+v", got)
	}

	// on_error ignore: failure is not a rejection and Fire keeps running
	// the remaining hooks.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	list := []Hook{
		{ID: "ping", Event: EventRunStart, Action: Action{Type: "http", URL: deadURL}, OnError: "ignore"},
		{ID: "note", Event: EventRunStart, Action: Action{Type: "prompt", Message: "after"}},
	}
	results := Fire(list, Context{Event: EventRunStart})
	if len(results) != 2 {
		t.Fatalf("expected both hooks to fire, got %+v", results)
	}
	if results[0].Success || results[0].Rejected || !strings.Contains(results[0].Output, "failed") {
		t.Fatalf("unexpected ignored failure: %+v", results[0])
	}
	if !results[1].Success || results[1].Output != "after" {
		t.Fatalf("subsequent hook interrupted: %+v", results[1])
	}
}
