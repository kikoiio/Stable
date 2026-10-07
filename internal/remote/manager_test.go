package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func startTestManager(t *testing.T, cfg Config) (*Manager, RemoteStatus) {
	t.Helper()
	manager := NewManager()
	status, err := manager.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Stop(ctx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	return manager, status
}

func TestRemoteManagerServesEmbeddedIndexAndStops(t *testing.T) {
	manager, status := startTestManager(t, Config{ListenAddress: "127.0.0.1:0"})
	if !status.Running || status.TLS || !strings.HasPrefix(status.ListenAddr, "127.0.0.1:") {
		t.Fatalf("Start() status = %+v", status)
	}
	response, err := http.Get("http://" + status.ListenAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Stable Remote") {
		t.Fatalf("GET / = %d %q", response.StatusCode, body)
	}
	if strings.Contains(string(body), remoteProviderCredentialMarker) {
		t.Fatal("embedded page exposed the provider credential marker")
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("embedded page did not set a content security policy")
	}
	for path, contentType := range map[string]string{"/ui/app.js": "text/javascript", "/ui/app.css": "text/css"} {
		asset, err := http.Get("http://" + status.ListenAddr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		assetBody, readErr := io.ReadAll(asset.Body)
		asset.Body.Close()
		if readErr != nil || asset.StatusCode != http.StatusOK || !strings.Contains(asset.Header.Get("Content-Type"), contentType) || len(assetBody) == 0 {
			t.Fatalf("GET %s = %d %q, err %v", path, asset.StatusCode, assetBody, readErr)
		}
		if strings.Contains(string(assetBody), remoteProviderCredentialMarker) {
			t.Fatalf("asset %s exposed the provider credential marker", path)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if got := manager.Status(); got.Running {
		t.Fatalf("status after Stop() = %+v", got)
	}
	if _, err := http.Get("http://" + status.ListenAddr + "/"); err == nil {
		t.Fatal("listener accepted a connection after Stop")
	}
}

func TestPairEndpointRequiresExactOriginAndSetsCookie(t *testing.T) {
	manager, status := startTestManager(t, Config{ListenAddress: "127.0.0.1:0"})
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + status.ListenAddr + "/api/pair"
	badTokenBody, _ := json.Marshal(map[string]string{"token": remoteProviderCredentialMarker})
	badTokenRequest, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(badTokenBody)))
	badTokenRequest.Header.Set("Origin", "http://"+status.ListenAddr)
	badTokenResponse, err := http.DefaultClient.Do(badTokenRequest)
	if err != nil {
		t.Fatal(err)
	}
	badTokenResponseBody, readErr := io.ReadAll(badTokenResponse.Body)
	badTokenResponse.Body.Close()
	if readErr != nil || badTokenResponse.StatusCode != http.StatusUnauthorized || strings.Contains(string(badTokenResponseBody), remoteProviderCredentialMarker) {
		t.Fatalf("failed pairing response exposed marker: status %d body %q err %v", badTokenResponse.StatusCode, badTokenResponseBody, readErr)
	}
	badRequest, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	badRequest.Header.Set("Origin", "http://attacker.example")
	badResponse, err := http.DefaultClient.Do(badRequest)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin pair status = %d", badResponse.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://"+status.ListenAddr)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin pair status = %d", response.StatusCode)
	}
	if len(response.Cookies()) != 1 {
		t.Fatalf("Set-Cookie count = %d", len(response.Cookies()))
	}
	cookie := response.Cookies()[0]
	if cookie.Name != pairingCookieName || !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("pair cookie = %+v", cookie)
	}
	if cookie.Value == token {
		t.Fatal("pair token was reused as browser cookie")
	}
	sessionRequest, _ := http.NewRequest(http.MethodGet, "http://"+status.ListenAddr+"/api/session", nil)
	sessionRequest.AddCookie(cookie)
	sessionResponse, err := http.DefaultClient.Do(sessionRequest)
	if err != nil {
		t.Fatal(err)
	}
	sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("authenticated session status = %d", sessionResponse.StatusCode)
	}
	spoofedHostRequest, _ := http.NewRequest(http.MethodGet, "http://"+status.ListenAddr+"/api/session", nil)
	_, activePort, _ := net.SplitHostPort(status.ListenAddr)
	spoofedHostRequest.Host = net.JoinHostPort("attacker.example", activePort)
	spoofedHostRequest.AddCookie(cookie)
	spoofedHostResponse, err := http.DefaultClient.Do(spoofedHostRequest)
	if err != nil {
		t.Fatal(err)
	}
	spoofedHostResponse.Body.Close()
	if spoofedHostResponse.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("spoofed Host status = %d, want misdirected request", spoofedHostResponse.StatusCode)
	}
}

func TestPairEndpointRejectsOversizedBody(t *testing.T) {
	_, status := startTestManager(t, Config{ListenAddress: "127.0.0.1:0"})
	endpoint := "http://" + status.ListenAddr + "/api/pair"
	request, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(strings.Repeat("x", maxPairBodyBytes+1)))
	request.Header.Set("Origin", "http://"+status.ListenAddr)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		t.Fatal("oversized pairing body was accepted")
	}
}

func TestRemoteManagerTLSUsesSecureCookie(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)
	manager, status := startTestManager(t, Config{ListenAddress: "0.0.0.0:0", CertFile: certPath, KeyFile: keyPath})
	if !status.TLS {
		t.Fatalf("TLS status = %+v", status)
	}
	_, port, err := net.SplitHostPort(status.ListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := net.JoinHostPort("127.0.0.1", port)
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequest(http.MethodPost, "https://"+clientAddr+"/api/pair", strings.NewReader(string(body)))
	request.Header.Set("Origin", "https://"+clientAddr)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("TLS pair status = %d", response.StatusCode)
	}
	if len(response.Cookies()) != 1 || !response.Cookies()[0].Secure {
		t.Fatalf("TLS pair cookies = %+v", response.Cookies())
	}
}

func TestRemoteRestartInvalidatesBrowserCookie(t *testing.T) {
	manager, first := startTestManager(t, Config{ListenAddress: "127.0.0.1:0"})
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	request, _ := http.NewRequest(http.MethodPost, "http://"+first.ListenAddr+"/api/pair", strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://"+first.ListenAddr)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(response.Cookies()) != 1 {
		t.Fatalf("pair cookies = %+v", response.Cookies())
	}
	cookie := response.Cookies()[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(context.Background(), Config{ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ = http.NewRequest(http.MethodGet, "http://"+second.ListenAddr+"/api/session", nil)
	request.AddCookie(cookie)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old browser cookie status = %d, want unauthorized", response.StatusCode)
	}
	newToken, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	newBody, _ := json.Marshal(map[string]string{"token": newToken})
	request, _ = http.NewRequest(http.MethodPost, "http://"+second.ListenAddr+"/api/pair", strings.NewReader(string(newBody)))
	request.Header.Set("Origin", "http://"+second.ListenAddr)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	newCookies := response.Cookies()
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(newCookies) != 1 {
		t.Fatalf("re-pair response = %d, cookies %v", response.StatusCode, newCookies)
	}
	request, _ = http.NewRequest(http.MethodGet, "http://"+second.ListenAddr+"/api/session", nil)
	request.AddCookie(newCookies[0])
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("new browser cookie status = %d, want no content", response.StatusCode)
	}
}

func TestWebSocketRequiresAuthenticationAndSameOrigin(t *testing.T) {
	manager, status := startTestManager(t, Config{ListenAddress: "127.0.0.1:0"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	baseURL := "http://" + status.ListenAddr
	wsURL := "ws://" + status.ListenAddr + "/ws"
	unauthenticatedHeader := http.Header{"Origin": []string{baseURL}}
	if _, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: unauthenticatedHeader}); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated WebSocket = response %v, error %v", response, err)
	}
	token, _, err := manager.IssuePairingToken()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	request, _ := http.NewRequest(http.MethodPost, baseURL+"/api/pair", strings.NewReader(string(body)))
	request.Header.Set("Origin", baseURL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cookies := response.Cookies()
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(cookies) != 1 {
		t.Fatalf("pair response = %d, cookies %v", response.StatusCode, cookies)
	}
	crossOriginHeader := http.Header{"Origin": []string{"http://attacker.example"}, "Cookie": []string{cookies[0].Name + "=" + cookies[0].Value}}
	if _, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: crossOriginHeader}); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin WebSocket = response %v, error %v", response, err)
	}
}
