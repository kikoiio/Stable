package remote

import (
	"context"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	pairingCookieName = "stable_remote_session"
	maxPairBodyBytes  = 16 << 10
)

//go:embed ui/*
var browserAssets embed.FS

type RemoteStatus struct {
	Running    bool   `json:"running"`
	ListenAddr string `json:"listen_addr,omitempty"`
	TLS        bool   `json:"tls"`
}

// Manager owns one same-origin HTTP/WebSocket server and its in-memory
// authentication state.
type Manager struct {
	mu          sync.Mutex
	cfg         Config
	server      *http.Server
	listener    net.Listener
	serveDone   chan struct{}
	serveErr    error
	chatSocket  string
	connections chan struct{}
	activeWS    map[*websocket.Conn]struct{}
	activeWG    sync.WaitGroup
	stopping    bool
	store       *pairingStore
	limiter     *PairRateLimiter
}

func NewManager(chatSocket ...string) *Manager {
	socket := ""
	if len(chatSocket) > 0 {
		socket = chatSocket[0]
	}
	return &Manager{
		store:       newPairingStore(defaultPairTTL),
		limiter:     NewPairRateLimiter(),
		chatSocket:  socket,
		connections: make(chan struct{}, 8),
		activeWS:    make(map[*websocket.Conn]struct{}),
	}
}

func (m *Manager) Start(_ context.Context, cfg Config) (RemoteStatus, error) {
	if err := cfg.Validate(); err != nil {
		return RemoteStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server != nil {
		return RemoteStatus{}, errors.New("remote service is already running")
	}
	listener, err := net.Listen("tcp", cfg.EffectiveListenAddress())
	if err != nil {
		return RemoteStatus{}, fmt.Errorf("listen for remote service: %w", err)
	}
	handler := m.handler()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	done := make(chan struct{})
	m.cfg, m.listener, m.server, m.serveDone = cfg, listener, server, done
	m.stopping = false
	m.serveErr = nil
	status := RemoteStatus{Running: true, ListenAddr: listener.Addr().String(), TLS: cfg.CertFile != ""}
	go func() {
		var serveErr error
		if cfg.CertFile != "" {
			serveErr = server.ServeTLS(listener, cfg.CertFile, cfg.KeyFile)
		} else {
			serveErr = server.Serve(listener)
		}
		if !errors.Is(serveErr, http.ErrServerClosed) {
			m.mu.Lock()
			m.serveErr = serveErr
			m.mu.Unlock()
		}
		close(done)
	}()
	return status, nil
}

func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	server := m.server
	if server == nil {
		m.mu.Unlock()
		return nil
	}
	done := m.serveDone
	store := m.store
	m.stopping = true
	connections := make([]*websocket.Conn, 0, len(m.activeWS))
	for conn := range m.activeWS {
		connections = append(connections, conn)
	}
	m.mu.Unlock()
	for _, conn := range connections {
		_ = conn.CloseNow()
	}

	err := server.Shutdown(ctx)
	if err != nil {
		_ = server.Close()
	}
	store.Clear()
	<-done
	activeDone := make(chan struct{})
	go func() { m.activeWG.Wait(); close(activeDone) }()
	select {
	case <-activeDone:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}

	m.mu.Lock()
	m.server = nil
	m.listener = nil
	m.serveDone = nil
	m.stopping = false
	serveErr := m.serveErr
	m.serveErr = nil
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if serveErr != nil {
		return serveErr
	}
	return nil
}

func (m *Manager) Status() RemoteStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil {
		return RemoteStatus{}
	}
	status := RemoteStatus{Running: true, TLS: m.cfg.CertFile != ""}
	if m.listener != nil {
		status.ListenAddr = m.listener.Addr().String()
	}
	return status
}

func (m *Manager) IssuePairingToken() (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.server == nil || m.stopping {
		return "", time.Time{}, errors.New("remote service is not running")
	}
	return m.store.Issue()
}

func (m *Manager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/pair", m.handlePair)
	mux.HandleFunc("GET /api/session", m.handleBrowserSession)
	mux.HandleFunc("GET /ws", m.handleWebSocket)
	mux.HandleFunc("GET /ui/", m.handleAsset)
	mux.HandleFunc("GET /", m.handleIndex)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.hostAllowed(r) {
			http.Error(w, "request rejected", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (m *Manager) hostAllowed(r *http.Request) bool {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	m.mu.Lock()
	cfg, listener := m.cfg, m.listener
	m.mu.Unlock()
	if listener == nil {
		return false
	}
	_, activePort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || port != activePort {
		return false
	}
	if r.TLS != nil {
		if r.TLS.ServerName != "" && !strings.EqualFold(host, r.TLS.ServerName) {
			return false
		}
		certificate, err := cfg.LoadTLSCertificate()
		if err != nil || len(certificate.Certificate) == 0 {
			return false
		}
		leaf := certificate.Leaf
		if leaf == nil {
			leaf, err = x509.ParseCertificate(certificate.Certificate[0])
		}
		return err == nil && leaf.VerifyHostname(host) == nil
	}
	requestIP := net.ParseIP(host)
	if requestIP == nil {
		return false
	}
	bindHost, _, err := net.SplitHostPort(cfg.EffectiveListenAddress())
	if err != nil {
		return false
	}
	bindIP := net.ParseIP(bindHost)
	if bindIP == nil {
		return false
	}
	if !bindIP.IsUnspecified() {
		return requestIP.Equal(bindIP)
	}
	return localInterfaceHasIP(requestIP)
}

func localInterfaceHasIP(want net.IP) bool {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		ipNet, ok := address.(*net.IPNet)
		if ok && ipNet.IP.Equal(want) {
			return true
		}
		ipAddr, ok := address.(*net.IPAddr)
		if ok && ipAddr.IP.Equal(want) {
			return true
		}
	}
	return false
}

func (m *Manager) handleBrowserSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(pairingCookieName)
	if err != nil || r.Host == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if _, ok := m.store.Authenticate(cookie.Value); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (m *Manager) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := browserAssets.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (m *Manager) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := browserAssets.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "remote UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (m *Manager) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return
	}
	source := remoteSource(r.RemoteAddr)
	if !m.limiter.Allow(source) {
		http.Error(w, "request rejected", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPairBodyBytes)
	var payload struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		m.limiter.Failure(source)
		http.Error(w, "pairing failed", http.StatusUnauthorized)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		m.limiter.Failure(source)
		http.Error(w, "pairing failed", http.StatusUnauthorized)
		return
	}
	session, err := m.store.Consume(payload.Token)
	if err != nil {
		m.limiter.Failure(source)
		http.Error(w, "pairing failed", http.StatusUnauthorized)
		return
	}
	secure := r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     pairingCookieName,
		Value:    session.Credential,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || r.Host == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}

func remoteSource(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}
