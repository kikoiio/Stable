package conversation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/sessionlog"
)

const (
	remoteAccessRequestTTL = 2 * time.Minute
	remoteAccessGrantTTL   = 8 * time.Hour
)

var (
	ErrRemoteAccessDenied   = errors.New("remote directory access denied")
	ErrRemoteAccessExpired  = errors.New("remote directory access request expired")
	ErrRemoteAccessClosed   = errors.New("remote access service is closed")
	ErrRemoteAccessCanceled = errors.New("remote directory access request canceled")
	ErrRemoteGrantInvalid   = errors.New("remote access grant is invalid or expired")
)

// RemoteAccessRequest is shown by the local TUI while it decides whether to
// grant one remote connection access to a canonical project directory.
type RemoteAccessRequest struct {
	ID          string    `json:"id"`
	ClientLabel string    `json:"client_label"`
	ProjectRoot string    `json:"project_root"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// ConnectionID is internal binding state and is never serialized to the UI.
	ConnectionID string `json:"-"`
}

// RemoteGrant binds one remote WebSocket connection to one canonical root.
// Its ID is opaque and must be treated as a bearer capability by the caller.
type RemoteGrant struct {
	ID          string    `json:"id"`
	ProjectRoot string    `json:"project_root"`
	ExpiresAt   time.Time `json:"expires_at"`
	// ConnectionID is checked by the registry and intentionally not sent to the
	// browser. The bridge binds the grant to the WebSocket that requested it.
	ConnectionID string `json:"-"`
}

type remoteAccessResult struct {
	grant RemoteGrant
	err   error
}

type remoteAccessPending struct {
	request RemoteAccessRequest
	done    chan struct{}
	result  remoteAccessResult
}

type remoteAccessGrantState struct {
	grant RemoteGrant
}

// RemoteAccessRegistry owns pending local approval requests and connection-
// scoped grants. All access fails closed after Close, timeout, or expiry.
type RemoteAccessRegistry struct {
	mu         sync.Mutex
	pending    map[string]*remoteAccessPending
	grants     map[string]remoteAccessGrantState
	closed     bool
	requestTTL time.Duration
	grantTTL   time.Duration
	now        func() time.Time
}

// NewRemoteAccessRegistry creates an in-memory access registry with a
// two-minute approval deadline and an eight-hour maximum grant lifetime.
func NewRemoteAccessRegistry() *RemoteAccessRegistry {
	return newRemoteAccessRegistry(remoteAccessRequestTTL, remoteAccessGrantTTL, time.Now)
}

func newRemoteAccessRegistry(requestTTL, grantTTL time.Duration, now func() time.Time) *RemoteAccessRegistry {
	if requestTTL <= 0 {
		requestTTL = remoteAccessRequestTTL
	}
	if grantTTL <= 0 {
		grantTTL = remoteAccessGrantTTL
	}
	if now == nil {
		now = time.Now
	}
	return &RemoteAccessRegistry{
		pending:    make(map[string]*remoteAccessPending),
		grants:     make(map[string]remoteAccessGrantState),
		requestTTL: requestTTL,
		grantTTL:   grantTTL,
		now:        now,
	}
}

// Request canonicalizes projectRoot, publishes a pending request for the
// local TUI, then waits for a local decision. Caller cancellation removes the
// pending request or any grant that raced with cancellation.
func (r *RemoteAccessRegistry) Request(ctx context.Context, connectionID, clientLabel, projectRoot string) (RemoteGrant, error) {
	return r.RequestWithID(ctx, "", connectionID, clientLabel, projectRoot)
}

// RequestWithID is Request with a caller-generated request ID. RemoteManager
// uses this form so it can cancel an approval waiter if its WebSocket closes.
func (r *RemoteAccessRegistry) RequestWithID(ctx context.Context, requestID, connectionID, clientLabel, projectRoot string) (RemoteGrant, error) {
	if strings.TrimSpace(connectionID) == "" {
		return RemoteGrant{}, errors.New("remote connection ID is required")
	}
	if len(requestID) > 128 {
		return RemoteGrant{}, errors.New("remote access request ID is too long")
	}
	root, err := sessionRoot(projectRoot)
	if err != nil {
		return RemoteGrant{}, fmt.Errorf("resolve remote project root: %w", err)
	}
	clientLabel = strings.TrimSpace(clientLabel)
	if clientLabel == "" {
		clientLabel = "Browser client"
	}
	if len(clientLabel) > 128 {
		clientLabel = clientLabel[:128]
	}

	if requestID == "" {
		requestID, err = randomRemoteAccessID()
		if err != nil {
			return RemoteGrant{}, err
		}
	}
	now := r.now()
	pending := &remoteAccessPending{
		request: RemoteAccessRequest{
			ID: requestID, ClientLabel: clientLabel, ProjectRoot: root,
			CreatedAt: now, ExpiresAt: now.Add(r.requestTTL), ConnectionID: connectionID,
		},
		done: make(chan struct{}),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return RemoteGrant{}, ErrRemoteAccessClosed
	}
	r.expireLocked(now)
	if _, exists := r.pending[requestID]; exists {
		r.mu.Unlock()
		return RemoteGrant{}, errors.New("remote access request ID is already pending")
	}
	r.pending[requestID] = pending
	r.mu.Unlock()

	timer := time.NewTimer(r.requestTTL)
	defer timer.Stop()
	select {
	case <-pending.done:
		if pending.result.err != nil {
			return RemoteGrant{}, pending.result.err
		}
		return pending.result.grant, nil
	case <-ctx.Done():
		r.cancelPending(requestID, pending)
		return RemoteGrant{}, ctx.Err()
	case <-timer.C:
		r.expireRequest(requestID, pending)
		return RemoteGrant{}, ErrRemoteAccessExpired
	}
}

// CancelRequest removes a pending request only for its owning remote
// connection, waking a waiter so a disconnected browser cannot leave a grant
// request behind until its normal approval timeout.
func (r *RemoteAccessRegistry) CancelRequest(requestID, connectionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRemoteAccessClosed
	}
	item, ok := r.pending[requestID]
	if !ok || connectionID == "" || item.request.ConnectionID != connectionID {
		return ErrRemoteAccessExpired
	}
	delete(r.pending, requestID)
	item.result.err = ErrRemoteAccessCanceled
	close(item.done)
	return nil
}

// Pending returns a stable, ID-sorted snapshot of requests that have not
// expired. It is intended for the local TUI polling path.
func (r *RemoteAccessRegistry) Pending() []RemoteAccessRequest {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	if r.closed {
		return nil
	}
	requests := make([]RemoteAccessRequest, 0, len(r.pending))
	for _, item := range r.pending {
		requests = append(requests, item.request)
	}
	sortRemoteAccessRequests(requests)
	return requests
}

func (s *Service) handleRemoteAccessMessage(ctx context.Context, c ClientMsg, updates chan ServerMsg) bool {
	if c.Op != "remote_access_request" && c.Op != "remote_access_list" && c.Op != "remote_access_resolve" && c.Op != "remote_access_release" && c.Op != "remote_access_cancel" {
		return false
	}
	if s.remoteAccess == nil {
		updates <- ServerMsg{Type: "error", Error: "remote directory approval is unavailable"}
		updates <- ServerMsg{Type: "done"}
		return true
	}
	respond := func(err error) {
		if err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
		}
		updates <- ServerMsg{Type: "done"}
	}
	switch c.Op {
	case "remote_access_request":
		if c.RemoteConnectionID == "" {
			respond(errors.New("remote connection identity is required"))
			return true
		}
		grant, err := s.remoteAccess.RequestWithID(ctx, c.RemoteAccessRequestID, c.RemoteConnectionID, c.RemoteClientLabel, c.ProjectRoot)
		if err != nil {
			respond(err)
			return true
		}
		updates <- ServerMsg{Type: "remote_grant", RemoteGrant: &grant}
		updates <- ServerMsg{Type: "done"}
	case "remote_access_list":
		if c.RemoteConnectionID != "" || c.RemoteGrantID != "" {
			respond(errors.New("remote access requests are visible only to the local TUI"))
			return true
		}
		updates <- ServerMsg{Type: "remote_access_requests", RemoteAccessRequests: s.remoteAccess.Pending()}
		updates <- ServerMsg{Type: "done"}
	case "remote_access_resolve":
		if c.RemoteConnectionID != "" || c.RemoteGrantID != "" {
			respond(errors.New("only the local TUI can approve remote directory access"))
			return true
		}
		respond(s.remoteAccess.Resolve(c.RemoteAccessRequestID, c.RemoteAccessDecision))
	case "remote_access_release":
		if c.RemoteConnectionID == "" {
			respond(errors.New("remote connection identity is required"))
			return true
		}
		respond(s.remoteAccess.Release(c.RemoteGrantID, c.RemoteConnectionID))
	case "remote_access_cancel":
		if c.RemoteConnectionID == "" {
			respond(errors.New("remote connection identity is required"))
			return true
		}
		respond(s.remoteAccess.CancelRequest(c.RemoteAccessRequestID, c.RemoteConnectionID))
	}
	return true
}

var remoteAllowedOperations = map[string]bool{
	"session_list": true, "session_create": true, "session_load": true,
	"session_search": true, "chat": true, "run_start": true,
	"run_subscribe": true, "run_cancel": true, "approval_list": true,
	"approval_resolve": true, "approval_cancel": true, "question_list": true,
	"reply": true, "plan_resolve": true,
}

func (s *Service) authorizeRemoteMessage(c *ClientMsg) (string, error) {
	if !remoteAllowedOperations[c.Op] {
		return "", fmt.Errorf("operation %q is unavailable to remote clients", c.Op)
	}
	if s.remoteAccess == nil {
		return "", ErrRemoteAccessClosed
	}
	grant, err := s.remoteAccess.Grant(c.RemoteGrantID, c.RemoteConnectionID)
	if err != nil {
		return "", err
	}
	c.ProjectRoot = grant.ProjectRoot
	c.Ephemeral = false
	if c.SessionID != "" && c.Op != "session_create" {
		if _, err := sessionlog.Replay(grant.ProjectRoot, c.SessionID); err != nil {
			return "", errors.New("session does not belong to the approved project directory")
		}
		s.bindRemoteSession(c.SessionID, grant.ProjectRoot)
	}
	if c.Op == "chat" && c.SessionID == "" {
		return "", errors.New("remote chat requires a session ID")
	}
	if c.Op == "run_start" && c.Run != nil {
		request := *c.Run
		request.Work.Kind = agent.WorkSession
		request.Work.SessionID = c.SessionID
		request.Work.GoalID = ""
		request.Work.WorkItemID = ""
		request.ProviderName = ""
		request.Model = ""
		request.AllowedScope = nil
		request.PermissionBounds = nil
		request.ResourceBounds = nil
		c.Run = &request
	}
	return grant.ProjectRoot, nil
}

func (s *Service) requestProjectRoot(c ClientMsg) (string, error) {
	if c.RemoteConnectionID == "" {
		return s.trustedSessionRoot(c.ProjectRoot)
	}
	if s.remoteAccess == nil {
		return "", ErrRemoteAccessClosed
	}
	grant, err := s.remoteAccess.Grant(c.RemoteGrantID, c.RemoteConnectionID)
	if err != nil {
		return "", err
	}
	provided, err := sessionRoot(c.ProjectRoot)
	if err != nil || provided != grant.ProjectRoot {
		return "", ErrRemoteGrantInvalid
	}
	return grant.ProjectRoot, nil
}

func (s *Service) bindRemoteSession(sessionID, projectRoot string) {
	if sessionID == "" || projectRoot == "" {
		return
	}
	s.mu.Lock()
	if s.remoteRoots == nil {
		s.remoteRoots = map[string]string{}
	}
	s.remoteRoots[sessionID] = projectRoot
	s.mu.Unlock()
}

func filterRemoteMessage(message *ServerMsg, root string) {
	if message == nil || root == "" {
		return
	}
	if len(message.Goals) > 0 {
		filtered := make([]core.Goal, 0, len(message.Goals))
		allowed := make(map[string]bool, len(message.Goals))
		for _, goal := range message.Goals {
			if goalBelongsToRoot(goal, root) {
				filtered = append(filtered, goal)
				allowed[goal.ID] = true
			}
		}
		message.Goals = filtered
		if len(message.GoalEvents) > 0 {
			events := make([]GoalEventSummary, 0, len(message.GoalEvents))
			for _, event := range message.GoalEvents {
				if allowed[event.GoalID] {
					events = append(events, event)
				}
			}
			message.GoalEvents = events
		}
	}
	if message.Goal != nil && !goalBelongsToRoot(*message.Goal, root) {
		message.Goal = nil
	}
}

func goalBelongsToRoot(goal core.Goal, root string) bool {
	if goal.AllowedRoot == "" || root == "" {
		return false
	}
	goalRoot, err := sessionRoot(goal.AllowedRoot)
	if err != nil {
		return false
	}
	return goalRoot == root
}

// Resolve applies one local TUI decision. Unknown, duplicate, and expired
// request IDs fail closed.
func (r *RemoteAccessRegistry) Resolve(requestID, decision string) error {
	if decision != "approve" && decision != "deny" {
		return fmt.Errorf("invalid remote access decision %q", decision)
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRemoteAccessClosed
	}
	r.expireLocked(now)
	item, ok := r.pending[requestID]
	if !ok {
		return ErrRemoteAccessExpired
	}
	delete(r.pending, requestID)
	if decision == "deny" {
		item.result.err = ErrRemoteAccessDenied
		close(item.done)
		return nil
	}
	grantID, err := randomRemoteAccessID()
	if err != nil {
		item.result.err = fmt.Errorf("create remote access grant: %w", err)
		close(item.done)
		return item.result.err
	}
	grant := RemoteGrant{
		ID: grantID, ProjectRoot: item.request.ProjectRoot,
		ExpiresAt: now.Add(r.grantTTL), ConnectionID: item.request.ConnectionID,
	}
	item.result.grant = grant
	r.grants[grantID] = remoteAccessGrantState{grant: grant}
	close(item.done)
	return nil
}

// Grant returns a live grant only when it belongs to the requesting
// connection. An expired or mismatched grant is never treated as authorized.
func (r *RemoteAccessRegistry) Grant(grantID, connectionID string) (RemoteGrant, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RemoteGrant{}, ErrRemoteAccessClosed
	}
	r.expireLocked(now)
	state, ok := r.grants[grantID]
	if !ok || connectionID == "" || state.grant.ConnectionID != connectionID {
		return RemoteGrant{}, ErrRemoteGrantInvalid
	}
	return state.grant, nil
}

// Release revokes a grant immediately. The owning connection ID is required
// so one remote connection cannot revoke another connection's grant.
func (r *RemoteAccessRegistry) Release(grantID, connectionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRemoteAccessClosed
	}
	state, ok := r.grants[grantID]
	if !ok || connectionID == "" || state.grant.ConnectionID != connectionID {
		return ErrRemoteGrantInvalid
	}
	delete(r.grants, grantID)
	return nil
}

// Close rejects all pending approvals and revokes every grant. It is safe to
// call more than once.
func (r *RemoteAccessRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for id, item := range r.pending {
		delete(r.pending, id)
		item.result.err = ErrRemoteAccessClosed
		close(item.done)
	}
	clear(r.grants)
}

func (r *RemoteAccessRegistry) cancelPending(id string, expected *remoteAccessPending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item, ok := r.pending[id]; ok && item == expected {
		delete(r.pending, id)
	}
	if expected.result.grant.ID != "" {
		delete(r.grants, expected.result.grant.ID)
	}
}

func (r *RemoteAccessRegistry) expireRequest(id string, expected *remoteAccessPending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item, ok := r.pending[id]; ok && item == expected {
		delete(r.pending, id)
		item.result.err = ErrRemoteAccessExpired
		close(item.done)
	}
	// If approval raced the timer, expiration wins for this waiter and its
	// newly-created grant is removed before the request returns.
	if expected.result.grant.ID != "" {
		delete(r.grants, expected.result.grant.ID)
	}
}

func (r *RemoteAccessRegistry) expireLocked(now time.Time) {
	for id, item := range r.pending {
		if !now.Before(item.request.ExpiresAt) {
			delete(r.pending, id)
			item.result.err = ErrRemoteAccessExpired
			close(item.done)
		}
	}
	for id, state := range r.grants {
		if !now.Before(state.grant.ExpiresAt) {
			delete(r.grants, id)
		}
	}
}

func randomRemoteAccessID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate remote access ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func sortRemoteAccessRequests(requests []RemoteAccessRequest) {
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
}
