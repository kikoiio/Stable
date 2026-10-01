package conversation

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/store"
)

type Deps struct {
	Store        *store.Store
	Provider     decision.StructuredProvider
	ChatProvider decision.ChatProvider
	Temporal     string
	ProjectRoot  string
	RunRoot      string
	SocketPath   string
	Refresher    core.DependencyRefresher
	PollEvery    time.Duration // goal status poll interval; 0 defaults to 2s
}

// Service is the persistent chat session: it owns the unix socket, fans out
// messages to every connected client, and translates user input into goal
// events. Client connections are thin terminals; all state lives here and in
// the store.
type Service struct {
	deps     Deps
	ln       net.Listener
	mu       sync.Mutex
	clients  map[chan ServerMsg]struct{}
	statuses map[string]core.GoalStatus
}

func Serve(ctx context.Context, deps Deps) (*Service, error) {
	if deps.PollEvery <= 0 {
		deps.PollEvery = 2 * time.Second
	}
	if err := os.Remove(deps.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", deps.SocketPath)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(deps.SocketPath, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Service{deps: deps, ln: ln, clients: map[chan ServerMsg]struct{}{}, statuses: map[string]core.GoalStatus{}}
	go s.pollGoals(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.acceptLoop(ctx)
	return s, nil
}

func (s *Service) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				continue
			}
			return
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Service) Close() error { return s.ln.Close() }

func (s *Service) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Replay the full history before streaming live updates, so a reconnecting
	// terminal sees a consistent transcript.
	history, err := s.deps.Store.ListMessages(ctx)
	if err != nil {
		_ = encodeServer(conn, ServerMsg{Type: "error", Error: err.Error()})
		return
	}
	for i := range history {
		if err = encodeServer(conn, ServerMsg{Type: "message", Message: &history[i]}); err != nil {
			return
		}
	}
	updates := make(chan ServerMsg, 64)
	s.mu.Lock()
	s.clients[updates] = struct{}{}
	s.mu.Unlock()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for msg := range updates {
			if err := encodeServer(conn, msg); err != nil {
				return
			}
		}
	}()

	s.readLoop(ctx, conn, updates)
	s.mu.Lock()
	delete(s.clients, updates)
	s.mu.Unlock()
	close(updates) // unblocks the writer
	<-writerDone
}

func (s *Service) readLoop(ctx context.Context, conn net.Conn, updates chan ServerMsg) error {
	dec := jsonDecoder(conn)
	for {
		var c ClientMsg
		if err := dec.Decode(&c); err != nil {
			return err
		}
		msgs, err := s.handle(ctx, c)
		for i := range msgs {
			s.broadcast(msgs[i])
		}
		if err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
		}
		// Queue completion after this client's broadcast messages so one-shot
		// clients can distinguish a finished request from a slow model response.
		updates <- ServerMsg{Type: "done"}
	}
}

func (s *Service) broadcast(msg ServerMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default: // drop for slow clients; history replay covers reconnects
		}
	}
}

// pollGoals pushes goal status changes to connected clients so terminals see
// progress without running goal status themselves.
func (s *Service) pollGoals(ctx context.Context) {
	tick := time.NewTicker(s.deps.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			continue
		}
		s.mu.Lock()
		for i := range goals {
			g := goals[i]
			if prev, ok := s.statuses[g.ID]; !ok || prev != g.Status {
				s.statuses[g.ID] = g.Status
				s.mu.Unlock()
				s.broadcast(ServerMsg{Type: "goal_update", Goal: &g})
				s.mu.Lock()
			}
		}
		s.mu.Unlock()
	}
}
