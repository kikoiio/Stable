package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpClient is shared by every http hook action. The transport mirrors
// http.DefaultTransport with conservative pooling bounds; no client-level
// Timeout is set because the overall request deadline comes from the hook's
// timeout, applied per call via runHTTPAction's context.
var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

const (
	// httpBodyReadCap bounds how much of a 2xx response body is read into
	// memory. It mirrors the 1 MiB sandbox OutputLimit precedent
	// (internal/execution) and is a memory guard only: hook output
	// truncation to sessionlog.MaxHookOutput is the caller's job
	// (conversation.HookGate.runOne), exactly as for command actions, so
	// this cap sits far above anything the caller would keep.
	httpBodyReadCap = 1 << 20
	// httpErrorBodyDigest caps the response body quoted in non-2xx error
	// messages so failure output stays compact.
	httpErrorBodyDigest = 512
)

// runHTTPAction performs one http hook action with the given overall
// timeout. URL and header values get os.ExpandEnv expansion; the body is
// sent verbatim; Method defaults to GET; Content-Type is only sent when the
// hook config supplies it via Headers. A 2xx response body (surrounding
// whitespace trimmed, like command output) becomes the hook output; any
// other status is an error carrying the status code and a truncated body
// digest. Timeout failures are reported with a message containing
// "timeout", distinguishable from general failures ("http action failed").
func runHTTPAction(a Action, timeout time.Duration) (output string, err error) {
	u, err := parseHTTPURL(os.ExpandEnv(a.URL))
	if err != nil {
		return "", err
	}
	if u == "" {
		// Kept word-compatible with the pre-M07-C stub so disabled-action
		// probing keeps reporting "not enabled"; config-level validation
		// (Validate) rejects unconfigured http hooks earlier with a
		// clearer message.
		return "", errors.New("http action not enabled (no url configured)")
	}
	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var body io.Reader
	if a.Body != "" {
		body = strings.NewReader(a.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", fmt.Errorf("http action failed: %w", err)
	}
	for k, v := range a.Headers {
		req.Header.Set(k, os.ExpandEnv(v))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return "", fmt.Errorf("http action timeout after %s: %w", timeout, err)
		}
		return "", fmt.Errorf("http action failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, httpErrorBodyDigest+1))
		if len(snippet) > httpErrorBodyDigest {
			snippet = snippet[:httpErrorBodyDigest]
		}
		return "", fmt.Errorf("http action %s %s returned status %d: body %q", method, u, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, httpBodyReadCap+1))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("http action timeout after %s: %w", timeout, err)
		}
		return "", fmt.Errorf("http action failed reading response body: %w", err)
	}
	if len(b) > httpBodyReadCap {
		b = b[:httpBodyReadCap]
	}
	return strings.TrimSpace(string(b)), nil
}
