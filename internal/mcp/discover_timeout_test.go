package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeUnderlyingConn 是内存双向连接：Write 只记录不阻塞，对端（模拟服务器）
// 通过 peerWrite 向客户端方向注入消息，Read 从中取用。
type fakeUnderlyingConn struct {
	mu        sync.Mutex
	writes    []jsonrpc.Message
	fromPeer  chan jsonrpc.Message
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeUnderlyingConn() *fakeUnderlyingConn {
	return &fakeUnderlyingConn{
		fromPeer: make(chan jsonrpc.Message),
		done:     make(chan struct{}),
	}
}

func (f *fakeUnderlyingConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case m := <-f.fromPeer:
		return m, nil
	case <-f.done:
		return nil, errors.New("fake conn closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeUnderlyingConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, msg)
	return nil
}

func (f *fakeUnderlyingConn) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

func (f *fakeUnderlyingConn) SessionID() string { return "fake" }

// written 返回已记录的底层写出消息（即直通结果）。
func (f *fakeUnderlyingConn) written() []jsonrpc.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jsonrpc.Message(nil), f.writes...)
}

// peerWrite 模拟服务器向客户端方向发出一条消息；阻塞直到被 pump 取走。
func (f *fakeUnderlyingConn) peerWrite(msg jsonrpc.Message) {
	f.fromPeer <- msg
}

type fakeTransport struct{ conn *fakeUnderlyingConn }

func (t *fakeTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return t.conn, nil
}

// newTestConn 组装被 discoverTimeoutTransport 包装的内存连接，
// 并把探测超时注入为 timeout，测试结束后恢复默认值并关闭连接。
func newTestConn(t *testing.T, timeout time.Duration) (mcp.Connection, *fakeUnderlyingConn) {
	t.Helper()
	old := discoverTimeout
	discoverTimeout = timeout
	t.Cleanup(func() { discoverTimeout = old })

	under := newFakeUnderlyingConn()
	wrapped := &discoverTimeoutTransport{Transport: &fakeTransport{conn: under}}
	conn, err := wrapped.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, under
}

func mustID(t *testing.T, v any) jsonrpc.ID {
	t.Helper()
	id, err := jsonrpc.MakeID(v)
	if err != nil {
		t.Fatalf("MakeID(%v) failed: %v", v, err)
	}
	return id
}

// readWithin 在时限内读一条消息；读到返回 (msg, nil)。
func readWithin(t *testing.T, conn mcp.Connection, d time.Duration) (jsonrpc.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return conn.Read(ctx)
}

// assertNothingRead 断言在时限内读不到任何消息（读到消息即失败）。
func assertNothingRead(t *testing.T, conn mcp.Connection, d time.Duration) {
	t.Helper()
	msg, err := readWithin(t, conn, d)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read within %v: got msg=%v err=%v, want deadline exceeded (no message)", d, msg, err)
	}
}

// a) 探测请求及时响应 → 直通，且超时后不再补发伪造响应。
func TestDiscoverTimeoutProbeAnsweredInTime(t *testing.T) {
	conn, under := newTestConn(t, 100*time.Millisecond)
	probeID := mustID(t, "probe-ok")

	req := &jsonrpc.Request{ID: probeID, Method: "server/discover"}
	if err := conn.Write(context.Background(), req); err != nil {
		t.Fatalf("Write probe: %v", err)
	}

	// 请求应原样直通到底层连接
	writes := under.written()
	if len(writes) != 1 || writes[0] != jsonrpc.Message(req) {
		t.Fatalf("underlying writes = %v, want exactly the original probe request", writes)
	}

	// 服务器在超时内回真实响应
	under.peerWrite(&jsonrpc.Response{ID: probeID, Result: json.RawMessage(`{"versions":["2025-06-18"]}`)})
	msg, err := readWithin(t, conn, time.Second)
	if err != nil {
		t.Fatalf("Read timely probe response: %v", err)
	}
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("got %T, want *jsonrpc.Response", msg)
	}
	if resp.ID != probeID || resp.Error != nil {
		t.Fatalf("probe response = %+v, want success response with ID %v", resp, probeID)
	}

	// 超时已过：探测已被响应，不应再补发伪造响应
	assertNothingRead(t, conn, 300*time.Millisecond)
}

// b) 探测请求超时 → 客户端侧收到伪造的 InternalError 响应。
func TestDiscoverTimeoutProbeTimesOut(t *testing.T) {
	conn, _ := newTestConn(t, 50*time.Millisecond)
	probeID := mustID(t, float64(1))

	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: probeID, Method: "server/discover"}); err != nil {
		t.Fatalf("Write probe: %v", err)
	}

	// 服务器一直不回应，超时后应收到伪造的错误响应
	msg, err := readWithin(t, conn, 2*time.Second)
	if err != nil {
		t.Fatalf("Read after probe timeout: %v", err)
	}
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		t.Fatalf("got %T, want *jsonrpc.Response", msg)
	}
	if resp.ID != probeID {
		t.Fatalf("fabricated response ID = %v, want %v", resp.ID, probeID)
	}
	var wireErr *jsonrpc.Error
	if !errors.As(resp.Error, &wireErr) {
		t.Fatalf("fabricated response error = %v, want *jsonrpc.Error", resp.Error)
	}
	if wireErr.Code != jsonrpc.CodeInternalError {
		t.Fatalf("fabricated error code = %d, want %d (InternalError)", wireErr.Code, jsonrpc.CodeInternalError)
	}
}

// c) 超时处理完再到的真实探测响应被丢弃，不产生第二次输出。
func TestDiscoverTimeoutLateResponseDropped(t *testing.T) {
	conn, under := newTestConn(t, 50*time.Millisecond)
	probeID := mustID(t, "probe-late")

	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: probeID, Method: "server/discover"}); err != nil {
		t.Fatalf("Write probe: %v", err)
	}

	// 等到伪造的超时响应
	msg, err := readWithin(t, conn, 2*time.Second)
	if err != nil {
		t.Fatalf("Read fabricated response: %v", err)
	}
	if resp, ok := msg.(*jsonrpc.Response); !ok || resp.ID != probeID || resp.Error == nil {
		t.Fatalf("got %+v, want fabricated error response for probe", msg)
	}

	// 这时迟到的真实响应才到，应被丢弃
	under.peerWrite(&jsonrpc.Response{ID: probeID, Result: json.RawMessage(`{"versions":["2025-06-18"]}`)})
	assertNothingRead(t, conn, 200*time.Millisecond)
}

// d) 普通请求不受探测超时逻辑影响。
func TestDiscoverTimeoutNormalRequestUnaffected(t *testing.T) {
	conn, under := newTestConn(t, 50*time.Millisecond)

	// 有响应的普通请求：原样直通、响应照常返回
	callID := mustID(t, "call-1")
	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: callID, Method: "tools/list"}); err != nil {
		t.Fatalf("Write normal request: %v", err)
	}
	under.peerWrite(&jsonrpc.Response{ID: callID, Result: json.RawMessage(`{"tools":[]}`)})
	msg, err := readWithin(t, conn, time.Second)
	if err != nil {
		t.Fatalf("Read normal response: %v", err)
	}
	if resp, ok := msg.(*jsonrpc.Response); !ok || resp.ID != callID || resp.Error != nil {
		t.Fatalf("got %+v, want success response for %v", msg, callID)
	}
	// 超时已过：普通请求绝不该触发伪造响应
	assertNothingRead(t, conn, 200*time.Millisecond)

	// 无响应的普通请求：超时后也不补发伪造响应
	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: mustID(t, "call-2"), Method: "ping"}); err != nil {
		t.Fatalf("Write unanswered normal request: %v", err)
	}
	assertNothingRead(t, conn, 200*time.Millisecond)

	// 两条普通请求都应原样到达底层
	writes := under.written()
	if len(writes) != 2 {
		t.Fatalf("underlying writes = %d messages, want 2", len(writes))
	}
	if req, ok := writes[0].(*jsonrpc.Request); !ok || req.Method != "tools/list" {
		t.Fatalf("writes[0] = %v, want tools/list request", writes[0])
	}
	if req, ok := writes[1].(*jsonrpc.Request); !ok || req.Method != "ping" {
		t.Fatalf("writes[1] = %v, want ping request", writes[1])
	}
}
