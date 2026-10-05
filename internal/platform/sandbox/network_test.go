package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"stable/internal/permission"
)

type mapResolver struct {
	mu     sync.RWMutex
	byHost map[string][]netip.Addr
}

type pipeDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *pipeDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, network+" "+address)
	d.mu.Unlock()
	local, remote := net.Pipe()
	go func() {
		defer remote.Close()
		_, _ = io.Copy(remote, remote)
	}()
	return local, nil
}

func (r *mapResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ips, ok := r.byHost[host]
	if !ok {
		return nil, fmt.Errorf("unknown host %s", host)
	}
	return append([]netip.Addr(nil), ips...), nil
}

func (r *mapResolver) set(host string, ips ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parsed := make([]netip.Addr, 0, len(ips))
	for _, value := range ips {
		parsed = append(parsed, netip.MustParseAddr(value))
	}
	r.byHost[host] = parsed
}

func TestNetworkGrantPinsExactResolvedTarget(t *testing.T) {
	resolver := &mapResolver{byHost: map[string][]netip.Addr{}}
	resolver.set("target.test", "127.0.0.1")
	grant, err := PinNetworkGrant(context.Background(), permission.NetworkGrant{Protocol: "tcp", Host: "TARGET.TEST.", Port: 8443}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Host != "target.test" || len(grant.ResolvedIPs) != 1 || grant.ResolvedIPs[0] != "127.0.0.1" {
		t.Fatalf("grant was not canonicalized and pinned: %+v", grant)
	}
	if err = ValidateNetworkTarget(grant, "tcp", "target.test", 8443, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); err != nil {
		t.Fatal("exact target should pass:", err)
	}
	for name, args := range map[string]struct {
		protocol string
		host     string
		port     uint16
		ips      []netip.Addr
	}{
		"protocol":   {"udp", "target.test", 8443, []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"host":       {"tcp", "other.test", 8443, []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"port":       {"tcp", "target.test", 8444, []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		"resolution": {"tcp", "target.test", 8443, []netip.Addr{netip.MustParseAddr("127.0.0.2")}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateNetworkTarget(grant, args.protocol, args.host, args.port, args.ips); err == nil {
				t.Fatal("changed target accepted")
			}
		})
	}
	if _, err = PinNetworkGrant(context.Background(), permission.NetworkGrant{Protocol: "tcp", Host: "target.test", Port: 80}, nil); err == nil {
		t.Fatal("grant without a resolver was accepted")
	}
}

func TestTrustedProxyPinsAndRechecksBeforeDial(t *testing.T) {
	resolver := &mapResolver{byHost: map[string][]netip.Addr{}}
	resolver.set("target.test", "127.0.0.1")
	grant, err := PinNetworkGrant(context.Background(), permission.NetworkGrant{Protocol: "tcp", Host: "target.test", Port: 8443}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &pipeDialer{}
	proxy := NetworkProxy{Grants: []permission.NetworkGrant{grant}, Resolver: resolver, Dialer: dialer}
	conn, response := trustedProxyRequest(t, proxy, proxyRequest{Protocol: "tcp", Host: "target.test", Port: 8443}, "pinned")
	if !response.OK || string(conn) != "pinned" {
		t.Fatalf("pinned target did not connect: response=%+v echo=%q", response, conn)
	}
	if len(dialer.addrs) != 1 || dialer.addrs[0] != "tcp 127.0.0.1:8443" {
		t.Fatalf("proxy dialed an unpinned target: %v", dialer.addrs)
	}
	if _, response = trustedProxyRequest(t, proxy, proxyRequest{Protocol: "tcp", Host: "other.test", Port: 8443}, ""); response.OK {
		t.Fatal("unapproved host was authorized")
	}
	if _, response = trustedProxyRequest(t, proxy, proxyRequest{Protocol: "tcp", Host: "target.test", Port: 8444}, ""); response.OK {
		t.Fatal("unapproved port was authorized")
	}
	resolver.set("target.test", "127.0.0.2")
	if _, response = trustedProxyRequest(t, proxy, proxyRequest{Protocol: "tcp", Host: "target.test", Port: 8443}, ""); response.OK {
		t.Fatal("changed DNS answer was authorized")
	}
	if len(dialer.addrs) != 1 {
		t.Fatalf("unauthorized attempts reached the dialer: %v", dialer.addrs)
	}
}

func trustedProxyRequest(t *testing.T, proxy NetworkProxy, request proxyRequest, payload string) ([]byte, proxyResponse) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		proxy.handle(context.Background(), server)
		close(done)
	}()
	if err := json.NewEncoder(client).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response proxyResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	if response.OK && payload != "" {
		if _, err := io.WriteString(client, payload); err != nil {
			t.Fatal(err)
		}
		echoed = make([]byte, len(payload))
		if _, err := io.ReadFull(client, echoed); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("trusted proxy handler did not stop")
	}
	return echoed, response
}

func TestNetworkProxyHTTPConnectAndSOCKS5(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("host policy does not permit local TCP listeners: %v", err)
		}
		t.Fatal(err)
	}
	defer target.Close()
	port := uint16(target.Addr().(*net.TCPAddr).Port)
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			conn, acceptErr := target.Accept()
			if acceptErr != nil {
				return
			}
			accepted <- struct{}{}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	resolver := &mapResolver{byHost: map[string][]netip.Addr{}}
	resolver.set("target.test", "127.0.0.1")
	grant, err := PinNetworkGrant(context.Background(), permission.NetworkGrant{Protocol: "tcp", Host: "target.test", Port: port}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	socketPath := filepath.Join(root, "proxy.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- (NetworkProxy{SocketPath: socketPath, Grants: []permission.NetworkGrant{grant}, Resolver: resolver}).Serve(ctx)
	}()
	waitForPath(t, socketPath)

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("host policy does not permit local TCP listeners: %v", err)
		}
		t.Fatal(err)
	}
	defer local.Close()
	localDone := make(chan error, 1)
	go func() { localDone <- ServeLocalProxy(ctx, local, socketPath) }()

	connectHTTP(t, local.Addr().String(), "target.test", port, "http-connect")
	connectSOCKS5(t, local.Addr().String(), "target.test", port, "socks-connect")
	for range 2 {
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("approved proxy target did not receive a connection")
		}
	}

	if status := deniedHTTPStatus(t, local.Addr().String(), "other.test", port); status != "403" {
		t.Fatalf("unauthorized host returned HTTP status %s", status)
	}
	if status := deniedHTTPStatus(t, local.Addr().String(), "target.test", port+1); status != "403" {
		t.Fatalf("unauthorized port returned HTTP status %s", status)
	}
	resolver.set("target.test", "127.0.0.2")
	if status := deniedHTTPStatus(t, local.Addr().String(), "target.test", port); status != "403" {
		t.Fatalf("changed DNS result returned HTTP status %s", status)
	}
	select {
	case <-accepted:
		t.Fatal("unauthorized proxy request reached the target")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	if err = <-proxyDone; err != nil {
		t.Fatal("trusted proxy did not stop on cancellation:", err)
	}
	if err = <-localDone; err != nil {
		t.Fatal("local proxy did not stop on cancellation:", err)
	}
	if _, err = os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("proxy socket was not removed: %v", err)
	}
}

func connectHTTP(t *testing.T, proxyAddr, host string, port uint16, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprintf(conn, "CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", host, port, host, port)
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "200 Connection Established") {
		t.Fatalf("CONNECT handshake failed: %q %v", line, err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err = io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(reader, got); err != nil || string(got) != payload {
		t.Fatalf("HTTP CONNECT tunnel echo=%q err=%v", got, err)
	}
}

func connectSOCKS5(t *testing.T, proxyAddr, host string, port uint16, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(conn)
	if _, err = conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err = io.ReadFull(reader, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("SOCKS5 method response=%v err=%v", method, err)
	}
	if len(host) > 255 {
		t.Fatal("host too long for test")
	}
	request := []byte{5, 1, 0, 3, byte(len(host))}
	request = append(request, []byte(host)...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], port)
	request = append(request, portBytes[:]...)
	if _, err = conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var response [10]byte
	if _, err = io.ReadFull(reader, response[:]); err != nil || response[1] != 0 {
		t.Fatalf("SOCKS5 connect response=%v err=%v", response, err)
	}
	if _, err = io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(reader, got); err != nil || string(got) != payload {
		t.Fatalf("SOCKS5 tunnel echo=%q err=%v", got, err)
	}
}

func deniedHTTPStatus(t *testing.T, proxyAddr, host string, port uint16) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprintf(conn, "CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", host, port, host, port)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if _, err = fmt.Sscanf(line, "HTTP/1.1 %s", &status); err != nil {
		t.Fatalf("invalid proxy denial response %q: %v", line, err)
	}
	return status
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatalf("path not created: %s", path)
}
