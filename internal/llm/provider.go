package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"stable/internal/appconfig"
)

type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type ToolUse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ToolResultPart struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role        string           `json:"role"`
	Content     string           `json:"content"`
	ToolUses    []ToolUse        `json:"tool_uses,omitempty"`
	ToolResults []ToolResultPart `json:"tool_results,omitempty"`
}

type Request struct {
	Model     string       `json:"model"`
	Messages  []Message    `json:"messages"`
	Tools     []ToolSchema `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

type EventKind string

const (
	TextDelta        EventKind = "text_delta"
	ThinkingDelta    EventKind = "thinking_delta"
	ThinkingComplete EventKind = "thinking_complete"
	ToolCallStart    EventKind = "tool_call_start"
	ToolCallDelta    EventKind = "tool_call_delta"
	ToolCallComplete EventKind = "tool_call_complete"
	Usage            EventKind = "usage"
	StreamEnd        EventKind = "stream_end"
)

type UsageInfo struct {
	InputTokens         *int `json:"input_tokens,omitempty"`
	OutputTokens        *int `json:"output_tokens,omitempty"`
	CacheReadTokens     *int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int `json:"cache_creation_tokens,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Complete  bool            `json:"complete"`
}

type Event struct {
	Kind       EventKind       `json:"kind"`
	Text       string          `json:"text,omitempty"`
	Tool       *ToolCall       `json:"tool,omitempty"`
	Usage      *UsageInfo      `json:"usage,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

type Provider interface {
	Stream(context.Context, Request) (<-chan Event, <-chan error)
}

type Config struct {
	Model      appconfig.ModelConfig
	HTTPClient interface {
		Do(*http.Request) (*http.Response, error)
	}
	MaxTokens int
	Now       func() time.Time
}

func streamChannels(ctx context.Context, run func(func(Event) error) error) (<-chan Event, <-chan error) {
	events := make(chan Event, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		err := run(func(event Event) error {
			select {
			case events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && ctx.Err() == nil {
			select {
			case errs <- safeStreamError(err):
			default:
			}
		}
	}()
	return events, errs
}

func safeStreamError(err error) error {
	if err == nil {
		return nil
	}
	if pe, ok := err.(*ProviderError); ok {
		return pe
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return networkError(err)
	}
	if errors.Is(err, errStreamInterrupted) {
		return &ProviderError{Class: ErrorNetwork, Message: "provider stream disconnected", Retryable: true}
	}
	return &ProviderError{Class: ErrorProvider, Message: "provider returned an invalid or incomplete stream"}
}

func postSSE(ctx context.Context, client interface {
	Do(*http.Request) (*http.Response, error)
}, endpoint string, body any, headers map[string]string, emit func(sseFrame) error) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return &ProviderError{Class: ErrorProvider, Message: "could not encode provider request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return &ProviderError{Class: ErrorProvider, Message: "invalid provider endpoint"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return networkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return classifyHTTPError(resp.StatusCode, resp.Header, body)
	}
	if err := readSSE(ctx, resp.Body, emit); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("stream: %w", err)
	}
	return nil
}

func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }
