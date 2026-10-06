package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"stable/internal/permission"
	"stable/internal/platform/ipc"
)

type GrantResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type NetworkDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type NetworkProxy struct {
	SocketPath string
	Grants     []permission.NetworkGrant
	Resolver   GrantResolver
	Dialer     NetworkDialer
}

type proxyRequest struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     uint16 `json:"port"`
}

type proxyResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// PinNetworkGrant resolves a user-approved endpoint once. The resulting IP set
// belongs in the trusted Authority and is rechecked by NetworkProxy on use.
func PinNetworkGrant(ctx context.Context, grant permission.NetworkGrant, resolver GrantResolver) (permission.NetworkGrant, error) {
	grant.Protocol = strings.ToLower(strings.TrimSpace(grant.Protocol))
	canonicalHost, err := normalizeHost(grant.Host)
	if err != nil || grant.Protocol != "tcp" || canonicalHost == "" || grant.Port == 0 || resolver == nil {
		return permission.NetworkGrant{}, networkGrantMessage("network grant must specify tcp, a host, a port, and a resolver")
	}
	grant.Host = canonicalHost
	ips, err := resolveGrant(ctx, grant.Host, resolver)
	if err != nil {
		return permission.NetworkGrant{}, fmt.Errorf("%w: %w: %v", ErrUnavailable, ErrNetworkGrantInvalid, err)
	}
	grant.ResolvedIPs = ipStrings(ips)
	return grant, nil
}

// validateNetworkGrants checks the trusted representation before a proxy is
// exposed. A proxy must never start with an unpinned or ambiguous grant set.
func validateNetworkGrants(grants []permission.NetworkGrant) error {
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		host, hostErr := normalizeHost(grant.Host)
		ips, ipErr := normalizedIPs(grant.ResolvedIPs)
		if hostErr != nil || ipErr != nil || grant.Host != host || grant.Protocol != "tcp" || grant.Port == 0 || len(ips) == 0 {
			return networkGrantMessage("network grant is incomplete or unpinned")
		}
		key := fmt.Sprintf("%s:%s:%d", grant.Protocol, host, grant.Port)
		if _, exists := seen[key]; exists {
			return networkGrantMessage("duplicate network grant")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func ValidateNetworkTarget(grant permission.NetworkGrant, protocol, host string, port uint16, resolved []netip.Addr) error {
	grantHost, grantErr := normalizeHost(grant.Host)
	targetHost, targetErr := normalizeHost(host)
	if grantErr != nil || targetErr != nil || grant.Protocol != "tcp" || protocol != "tcp" || grantHost != targetHost || grant.Port == 0 || grant.Port != port {
		return errors.New("network target does not match the approved protocol, host, and port")
	}
	want, err := normalizedIPs(grant.ResolvedIPs)
	if err != nil || len(want) == 0 {
		return errors.New("network grant has no valid pinned addresses")
	}
	got := normalizedIPAddrs(resolved)
	if len(got) == 0 || !sameStrings(want, got) {
		return errors.New("network resolution changed; approval must be renewed")
	}
	return nil
}

// Serve accepts one newline-delimited request per Unix connection and then
// tunnels bytes only to a pinned, explicitly approved TCP destination.
func (p NetworkProxy) Serve(ctx context.Context) error {
	if p.SocketPath == "" || !filepath.IsAbs(p.SocketPath) || p.Resolver == nil {
		return errors.New("network proxy requires an absolute socket path and resolver")
	}
	if err := validateNetworkGrants(p.Grants); err != nil {
		return err
	}
	if p.Dialer == nil {
		p.Dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	}
	if _, err := os.Lstat(p.SocketPath); err == nil {
		return errors.New("network proxy socket path already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := ipc.ListenPrivate(p.SocketPath, false)
	if err != nil {
		return err
	}
	createdInfo, err := os.Lstat(p.SocketPath)
	if err != nil {
		listener.Close()
		return err
	}
	defer func() {
		listener.Close()
		if current, statErr := os.Lstat(p.SocketPath); statErr == nil {
			if os.SameFile(createdInfo, current) {
				_ = os.Remove(p.SocketPath)
			}
		}
	}()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	var handlers sync.WaitGroup
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				handlers.Wait()
				return nil
			}
			return acceptErr
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			p.handle(ctx, conn)
		}()
	}
}

func (p NetworkProxy) handle(ctx context.Context, client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(io.LimitReader(client, 4096))
	var request proxyRequest
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "invalid proxy request"})
		return
	}
	host, err := normalizeHost(request.Host)
	if err != nil {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "invalid target host"})
		return
	}
	var matched *permission.NetworkGrant
	for i := range p.Grants {
		g := p.Grants[i]
		gh, hostErr := normalizeHost(g.Host)
		if hostErr == nil && g.Protocol == request.Protocol && gh == host && g.Port == request.Port {
			if matched != nil {
				_ = json.NewEncoder(client).Encode(proxyResponse{Error: "ambiguous network grant"})
				return
			}
			matched = &g
		}
	}
	if matched == nil {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "network destination is not authorized"})
		return
	}
	ips, err := resolveGrant(ctx, host, p.Resolver)
	if err != nil || ValidateNetworkTarget(*matched, request.Protocol, host, request.Port, ips) != nil {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "network resolution changed or is unavailable"})
		return
	}
	if len(ips) == 0 {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "network target has no addresses"})
		return
	}
	upstream, err := p.Dialer.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), strconv.Itoa(int(request.Port))))
	if err != nil {
		_ = json.NewEncoder(client).Encode(proxyResponse{Error: "approved network target is unavailable"})
		return
	}
	defer upstream.Close()
	_ = client.SetDeadline(time.Time{})
	if err = json.NewEncoder(client).Encode(proxyResponse{OK: true}); err != nil {
		return
	}
	tunnel(ctx, client, upstream)
}

// ServeLocalProxy runs inside the isolated network namespace. It exposes only
// HTTP CONNECT and SOCKS5 TCP proxy entry points and relays requests over the
// mounted Unix socket to the trusted host-side NetworkProxy.
func ServeLocalProxy(ctx context.Context, listener net.Listener, socketPath string) error {
	if listener == nil || socketPath == "" {
		return errors.New("local proxy requires a listener and trusted socket")
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	var handlers sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				handlers.Wait()
				return nil
			}
			return err
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			serveLocalConnection(ctx, conn, socketPath)
		}()
	}
}

// RunProxyCommand is the small trusted wrapper entry point in agentworker. It
// runs the caller's argv with proxy variables pointing only at a loopback
// listener, then stops that listener when the command exits.
func RunProxyCommand(ctx context.Context, socketPath string, argv []string) (int, error) {
	if socketPath == "" || len(argv) == 0 || argv[0] == "" {
		return 126, errors.New("sandbox proxy wrapper requires a socket and argv")
	}
	if err := enableLoopback(ctx); err != nil {
		return 126, fmt.Errorf("enable private loopback for the approved proxy: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 126, fmt.Errorf("start isolated loopback proxy: %w", err)
	}
	proxyContext, stopProxy := context.WithCancel(ctx)
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- ServeLocalProxy(proxyContext, listener, socketPath) }()
	defer func() {
		stopProxy()
		_ = listener.Close()
		<-proxyDone
	}()
	address := listener.Addr().String()
	httpProxy := "http://" + address
	socksProxy := "socks5h://" + address
	env := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		default:
			env = append(env, entry)
		}
	}
	env = append(env, "HTTP_PROXY="+httpProxy, "http_proxy="+httpProxy,
		"HTTPS_PROXY="+httpProxy, "https_proxy="+httpProxy,
		"ALL_PROXY="+socksProxy, "all_proxy="+socksProxy,
		"NO_PROXY=", "no_proxy=")
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env, command.Stdin, command.Stdout, command.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 126, err
}

func serveLocalConnection(ctx context.Context, conn net.Conn, socketPath string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		return
	}
	var upstream net.Conn
	if first[0] == 5 {
		upstream, err = socksConnect(ctx, conn, reader, socketPath)
	} else {
		upstream, err = httpConnect(ctx, conn, reader, socketPath)
	}
	if err != nil || upstream == nil {
		return
	}
	defer upstream.Close()
	_ = conn.SetDeadline(time.Time{})
	tunnel(ctx, conn, upstream)
}

func httpConnect(ctx context.Context, client net.Conn, reader *bufio.Reader, socketPath string) (net.Conn, error) {
	request, err := httpReadConnect(reader)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return nil, err
	}
	upstream, err := dialProxySocket(ctx, socketPath, request)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return nil, err
	}
	if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		upstream.Close()
		return nil, err
	}
	return upstream, nil
}

func httpReadConnect(reader *bufio.Reader) (proxyRequest, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return proxyRequest{}, err
	}
	if len(line) > 8192 {
		return proxyRequest{}, errors.New("HTTP CONNECT request line is too long")
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[0] != "CONNECT" || !strings.HasPrefix(parts[2], "HTTP/") {
		return proxyRequest{}, errors.New("only HTTP CONNECT is supported")
	}
	consumed := len(line)
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			return proxyRequest{}, err
		}
		consumed += len(line)
		if consumed > 16384 {
			return proxyRequest{}, errors.New("HTTP CONNECT headers are too large")
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	host, portText, err := net.SplitHostPort(parts[1])
	if err != nil {
		return proxyRequest{}, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return proxyRequest{}, errors.New("invalid HTTP CONNECT port")
	}
	return proxyRequest{Protocol: "tcp", Host: host, Port: uint16(port)}, nil
}

func socksConnect(ctx context.Context, client net.Conn, reader *bufio.Reader, socketPath string) (net.Conn, error) {
	var greeting [2]byte
	if _, err := io.ReadFull(reader, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] == 0 || greeting[1] > 32 {
		return nil, errors.New("invalid SOCKS5 greeting")
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return nil, err
	}
	noAuth := false
	for _, method := range methods {
		noAuth = noAuth || method == 0
	}
	if !noAuth {
		_, _ = client.Write([]byte{5, 0xff})
		return nil, errors.New("SOCKS5 no-auth method is required")
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return nil, err
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil || header[0] != 5 || header[1] != 1 || header[2] != 0 {
		return nil, errors.New("only SOCKS5 CONNECT is supported")
	}
	var host string
	switch header[3] {
	case 1:
		ip := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return nil, err
		}
		host = net.IP(ip).String()
	case 4:
		ip := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return nil, err
		}
		host = net.IP(ip).String()
	case 3:
		n, err := reader.ReadByte()
		if err != nil || n == 0 {
			return nil, errors.New("invalid SOCKS5 domain")
		}
		name := make([]byte, int(n))
		if _, err = io.ReadFull(reader, name); err != nil {
			return nil, err
		}
		host = string(name)
	default:
		return nil, errors.New("invalid SOCKS5 address type")
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(reader, portBytes[:]); err != nil {
		return nil, err
	}
	port := uint16(portBytes[0])<<8 | uint16(portBytes[1])
	upstream, err := dialProxySocket(ctx, socketPath, proxyRequest{Protocol: "tcp", Host: host, Port: port})
	if err != nil {
		_, _ = client.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
		return nil, err
	}
	if _, err = client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		upstream.Close()
		return nil, err
	}
	return upstream, nil
}

func dialProxySocket(ctx context.Context, socketPath string, request proxyRequest) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	var response proxyResponse
	if err = json.NewDecoder(reader).Decode(&response); err != nil {
		conn.Close()
		return nil, err
	}
	if !response.OK {
		conn.Close()
		return nil, fmt.Errorf("trusted proxy denied connection: %s", response.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func tunnel(ctx context.Context, left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyHalf := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyHalf(left, right)
	go copyHalf(right, left)
	select {
	case <-ctx.Done():
		_ = left.Close()
		_ = right.Close()
	case <-done:
		_ = left.SetDeadline(time.Now())
		_ = right.SetDeadline(time.Now())
		<-done
	}
}

func resolveGrant(ctx context.Context, host string, resolver GrantResolver) ([]netip.Addr, error) {
	canonical, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}
	if ip, parseErr := netip.ParseAddr(canonical); parseErr == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", canonical)
	if err != nil {
		return nil, fmt.Errorf("resolve approved host: %w", err)
	}
	if len(ips) == 0 {
		return nil, errors.New("approved host has no resolved addresses")
	}
	for i := range ips {
		ips[i] = ips[i].Unmap()
		if !ips[i].IsValid() || ips[i].IsUnspecified() || ips[i].IsMulticast() {
			return nil, errors.New("approved host resolved to an invalid address")
		}
	}
	sort.Slice(ips, func(i, j int) bool { return ips[i].Less(ips[j]) })
	unique := ips[:0]
	for _, ip := range ips {
		if len(unique) == 0 || unique[len(unique)-1] != ip {
			unique = append(unique, ip)
		}
	}
	return unique, nil
}

func normalizedIPAddrs(ips []netip.Addr) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.IsValid() {
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func normalizedIPs(ips []string) ([]string, error) {
	out := make([]string, 0, len(ips))
	for _, value := range ips {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, errors.New("invalid pinned network address")
		}
		out = append(out, ip.Unmap().String())
	}
	sort.Strings(out)
	return uniqueStrings(out), nil
}

func ipStrings(ips []netip.Addr) []string { return normalizedIPAddrs(ips) }

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
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

func normalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || strings.ContainsAny(host, " /\\\t\r\n\x00") {
		return "", errors.New("invalid network host")
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		return "", errors.New("network hosts must not include brackets")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String(), nil
	}
	if len(host) > 253 {
		return "", errors.New("network host name is too long")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid network host name")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '-' {
				return "", errors.New("invalid network host name")
			}
		}
	}
	return host, nil
}
