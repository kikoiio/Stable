package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ErrorClass string

const (
	ErrorAuth         ErrorClass = "auth"
	ErrorRateLimit    ErrorClass = "rate_limit"
	ErrorNetwork      ErrorClass = "network"
	ErrorContextLimit ErrorClass = "context_limit"
	ErrorProvider     ErrorClass = "provider"
)

type ProviderError struct {
	Class      ErrorClass
	Message    string
	RetryAfter time.Duration
	Retryable  bool
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "provider error"
	}
	if e.Message == "" {
		return string(e.Class)
	}
	return e.Message
}

func classifyHTTPError(status int, header http.Header, body []byte) *ProviderError {
	class := ErrorProvider
	retryable := status >= 500
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		class, retryable = ErrorAuth, false
	case http.StatusTooManyRequests:
		class, retryable = ErrorRateLimit, true
	}
	var detail struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &detail)
	text := strings.ToLower(detail.Error.Type + " " + detail.Error.Code + " " + detail.Error.Message)
	if strings.Contains(text, "context_length") || strings.Contains(text, "context length") || strings.Contains(text, "maximum context") || strings.Contains(text, "too many tokens") {
		class, retryable = ErrorContextLimit, false
	}
	message := fmt.Sprintf("provider request failed (HTTP %d)", status)
	if class == ErrorAuth {
		message = "provider authentication failed"
	} else if class == ErrorRateLimit {
		message = "provider rate limit reached"
	} else if class == ErrorContextLimit {
		message = "provider context limit exceeded"
	}
	return &ProviderError{Class: class, Message: message, RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now()), Retryable: retryable}
}

func networkError(err error) *ProviderError {
	if errors.Is(err, context.Canceled) {
		return &ProviderError{Class: ErrorNetwork, Message: "provider request cancelled"}
	}
	return &ProviderError{Class: ErrorNetwork, Message: "provider network request failed", Retryable: true}
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
