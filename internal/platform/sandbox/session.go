package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"stable/internal/platform/ipc"
	"stable/internal/platform/proc"
)

type sessionState struct {
	ID         string `json:"id"`
	Candidate  string `json:"candidate_id"`
	Generation uint64 `json:"generation"`
}

type managedSession struct {
	value       SandboxSession
	cmd         *exec.Cmd
	done        chan struct{}
	cancel      context.CancelFunc
	exitErr     error
	rpcMu       sync.Mutex
	conn        net.Conn
	reader      *bufio.Reader
	cleanup     func()
	cleanupOnce sync.Once
}

type sessionControlReady struct {
	conn   net.Conn
	reader *bufio.Reader
	err    error
}

var activeSessions = struct {
	sync.Mutex
	byID     map[string]*managedSession
	starting map[string]bool
}{byID: map[string]*managedSession{}, starting: map[string]bool{}}

func (m LinuxManager) StartIsolatedSession(ctx context.Context, profile SandboxProfile) (SandboxSession, error) {
	if len(profile.NetworkGrants) != 0 {
		return SandboxSession{}, errors.New("network grants are not supported for persistent sessions")
	}
	if profile.CandidateID == "" || len(profile.SessionArgv) == 0 {
		return SandboxSession{}, errors.New("isolated session requires a candidate ID and supervised argv")
	}
	if err := m.Probe(ctx, profile); err != nil {
		return SandboxSession{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := ValidateProfile(profile); err != nil {
		return SandboxSession{}, err
	}
	id := profile.SessionID
	if id == "" {
		var err error
		id, err = newSessionID()
		if err != nil {
			return SandboxSession{}, err
		}
	}
	activeSessions.Lock()
	if old := activeSessions.byID[id]; old != nil {
		select {
		case <-old.done:
			delete(activeSessions.byID, id)
		default:
			activeSessions.Unlock()
			return SandboxSession{}, errors.New("isolated session ID is already active")
		}
	}
	if activeSessions.starting[id] {
		activeSessions.Unlock()
		return SandboxSession{}, errors.New("isolated session ID is already active")
	}
	activeSessions.starting[id] = true
	activeSessions.Unlock()
	defer func() {
		activeSessions.Lock()
		delete(activeSessions.starting, id)
		activeSessions.Unlock()
	}()
	generation, err := nextSessionGeneration(profile.RunRoot, id, profile.CandidateID)
	if err != nil {
		return SandboxSession{}, err
	}
	controlDir, _, listener, err := createSessionControlSocket(profile.RunRoot)
	if err != nil {
		return SandboxSession{}, err
	}
	cleanupControl := func() {
		_ = listener.Close()
		_ = os.RemoveAll(controlDir)
	}
	profile.ReadOnlyMounts = append(profile.ReadOnlyMounts, ReadOnlyMount{HostPath: controlDir, GuestPath: "/run/stable-session"})
	profile.sessionControl = true
	profile.sessionGeneration = generation
	args, err := m.args(profile, profile.SessionArgv)
	if err != nil {
		cleanupControl()
		return SandboxSession{}, err
	}
	sessionCtx, cancel := context.WithCancel(context.Background())
	cmd := m.command(sessionCtx, args[0], args[1:]...)
	if err = proc.ConfigureChild(cmd); err != nil {
		cancel()
		cleanupControl()
		return SandboxSession{}, err
	}
	cmd.Stdin = nil
	logFile, err := openSessionLog(profile.RunRoot, id)
	if err != nil {
		cancel()
		cleanupControl()
		return SandboxSession{}, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		logFile.Close()
		cancel()
		cleanupControl()
		return SandboxSession{}, fmt.Errorf("start isolated session: %w", err)
	}
	result := SandboxSession{ID: id, Generation: generation, CandidateID: profile.CandidateID, PID: cmd.Process.Pid}
	managed := &managedSession{value: result, cmd: cmd, done: make(chan struct{}), cancel: cancel, cleanup: cleanupControl}
	go func() {
		managed.exitErr = cmd.Wait()
		_ = logFile.Close()
		close(managed.done)
	}()
	activeSessions.Lock()
	delete(activeSessions.starting, id)
	activeSessions.byID[id] = managed
	activeSessions.Unlock()
	ready := make(chan sessionControlReady, 1)
	go func() { ready <- awaitSessionControl(listener) }()
	select {
	case <-managed.done:
		activeSessions.Lock()
		delete(activeSessions.byID, id)
		activeSessions.Unlock()
		cancel()
		managed.cleanupOnce.Do(managed.cleanup)
		if managed.exitErr == nil {
			return SandboxSession{}, errors.New("isolated session exited during startup")
		}
		return SandboxSession{}, fmt.Errorf("isolated session exited during startup: %w", managed.exitErr)
	case accepted := <-ready:
		if accepted.err != nil {
			_ = stopManagedSession(context.Background(), managed)
			activeSessions.Lock()
			delete(activeSessions.byID, id)
			activeSessions.Unlock()
			return SandboxSession{}, fmt.Errorf("isolated session control socket failed: %w", accepted.err)
		}
		_ = listener.Close()
		managed.rpcMu.Lock()
		managed.conn, managed.reader = accepted.conn, accepted.reader
		managed.rpcMu.Unlock()
		return result, nil
	case <-time.After(30 * time.Second):
		_ = stopManagedSession(context.Background(), managed)
		activeSessions.Lock()
		delete(activeSessions.byID, id)
		activeSessions.Unlock()
		return SandboxSession{}, errors.New("isolated session did not become ready")
	case <-ctx.Done():
		_ = stopManagedSession(context.Background(), managed)
		activeSessions.Lock()
		delete(activeSessions.byID, id)
		activeSessions.Unlock()
		return SandboxSession{}, ctx.Err()
	}
}

func (LinuxManager) CallIsolatedSession(ctx context.Context, session SandboxSession, input io.Reader) (SandboxResult, error) {
	var result SandboxResult
	if err := ValidateSession(session); err != nil {
		return result, err
	}
	activeSessions.Lock()
	managed := activeSessions.byID[session.ID]
	activeSessions.Unlock()
	if managed == nil {
		return result, errors.New("isolated session is not active")
	}
	request, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return result, err
	}
	if len(request) == 0 || len(request) > 1<<20 || !json.Valid(bytes.TrimSpace(request)) {
		return result, errors.New("isolated session request is invalid or too large")
	}
	if request[len(request)-1] != '\n' {
		request = append(request, '\n')
	}
	managed.rpcMu.Lock()
	defer managed.rpcMu.Unlock()
	if managed.conn == nil || managed.reader == nil {
		return result, errors.New("isolated session control connection is unavailable")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = managed.conn.SetDeadline(deadline)
	} else {
		_ = managed.conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	stopDeadline := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = managed.conn.SetDeadline(time.Now())
		case <-stopDeadline:
		}
	}()
	defer close(stopDeadline)
	if _, err = managed.conn.Write(request); err != nil {
		return result, err
	}
	response, err := readSessionLine(managed.reader, 1<<20)
	if err != nil {
		return result, err
	}
	if !json.Valid(bytes.TrimSpace(response)) {
		return result, errors.New("isolated session returned invalid JSON")
	}
	result.Stdout = response
	return result, nil
}

func (LinuxManager) StopIsolatedSession(ctx context.Context, id string) error {
	activeSessions.Lock()
	managed := activeSessions.byID[id]
	activeSessions.Unlock()
	if managed == nil {
		return errors.New("isolated session is not active in this worker")
	}
	if err := stopManagedSession(ctx, managed); err != nil {
		return err
	}
	activeSessions.Lock()
	if activeSessions.byID[id] == managed {
		delete(activeSessions.byID, id)
	}
	activeSessions.Unlock()
	return nil
}

func ValidateSession(session SandboxSession) error {
	activeSessions.Lock()
	managed := activeSessions.byID[session.ID]
	activeSessions.Unlock()
	if managed == nil || managed.value.Generation != session.Generation || managed.value.CandidateID != session.CandidateID {
		return errors.New("isolated session handle is stale")
	}
	select {
	case <-managed.done:
		return errors.New("isolated session process has exited")
	default:
		return nil
	}
}

func stopManagedSession(ctx context.Context, managed *managedSession) error {
	managed.cancel()
	managed.rpcMu.Lock()
	if managed.conn != nil {
		_ = managed.conn.Close()
		managed.conn = nil
	}
	managed.rpcMu.Unlock()
	defer managed.cleanupOnce.Do(managed.cleanup)
	if managed.cmd.Process == nil {
		return nil
	}
	pid := managed.cmd.Process.Pid
	_ = proc.KillGroup(pid, false)
	select {
	case <-managed.done:
		_ = proc.KillGroup(pid, true)
		return nil
	case <-ctx.Done():
		_ = proc.KillGroup(pid, true)
		select {
		case <-managed.done:
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return fmt.Errorf("isolated session process %d did not stop", pid)
		}
	case <-time.After(2 * time.Second):
		_ = proc.KillGroup(pid, true)
		select {
		case <-managed.done:
			return nil
		case <-time.After(2 * time.Second):
			return fmt.Errorf("isolated session process %d did not stop", pid)
		}
	}
}

func createSessionControlSocket(runRoot string) (string, string, net.Listener, error) {
	// Unix socket addresses are limited to roughly 108 bytes on Linux. The
	// worker run root can be deeply nested under a temporary directory, so keep
	// the control socket in a short private temporary parent and mount that
	// directory into the sandbox below /run/stable-session.
	dir, err := os.MkdirTemp("", ".stable-session-control-")
	if err != nil {
		return "", "", nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	path := filepath.Join(dir, "session.sock")
	listener, err := ipc.ListenPrivate(path, false)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	return dir, path, listener, nil
}

func awaitSessionControl(listener net.Listener) sessionControlReady {
	conn, err := listener.Accept()
	if err != nil {
		return sessionControlReady{err: err}
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := readSessionLine(reader, 4096)
	if err != nil {
		_ = conn.Close()
		return sessionControlReady{err: err}
	}
	var ready struct {
		Ready bool `json:"ready"`
	}
	if err = json.Unmarshal(line, &ready); err != nil || !ready.Ready {
		_ = conn.Close()
		return sessionControlReady{err: errors.New("session ready handshake is invalid")}
	}
	_ = conn.SetDeadline(time.Time{})
	return sessionControlReady{conn: conn, reader: reader}
}

func readSessionLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return nil, errors.New("isolated session line exceeds limit")
		}
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func nextSessionGeneration(runRoot, id, candidate string) (uint64, error) {
	root, err := filepath.Abs(runRoot)
	if err != nil {
		return 0, err
	}
	stateDir := filepath.Join(filepath.Dir(root), ".stable-sessions")
	if err = os.MkdirAll(stateDir, 0700); err != nil {
		return 0, err
	}
	if err = os.Chmod(stateDir, 0700); err != nil {
		return 0, err
	}
	sum := sha256.Sum256([]byte(id))
	path := filepath.Join(stateDir, hex.EncodeToString(sum[:16])+".json")
	state := sessionState{ID: id, Candidate: candidate}
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if err = json.Unmarshal(raw, &state); err != nil || state.ID != id {
			return 0, errors.New("isolated session generation record is corrupt")
		}
	} else if !os.IsNotExist(readErr) {
		return 0, readErr
	}
	state.ID, state.Candidate = id, candidate
	state.Generation++
	if state.Generation == 0 {
		return 0, errors.New("isolated session generation overflow")
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	temp, err := os.CreateTemp(stateDir, ".session-*.tmp")
	if err != nil {
		return 0, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(encoded)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	if err = os.Rename(tempName, path); err != nil {
		return 0, err
	}
	return state.Generation, nil
}

func openSessionLog(runRoot, id string) (*os.File, error) {
	root, err := filepath.Abs(runRoot)
	if err != nil {
		return nil, err
	}
	logDir := filepath.Join(filepath.Dir(root), ".stable-sessions", "logs")
	if err = os.MkdirAll(logDir, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(logDir, 0700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(id))
	return os.OpenFile(filepath.Join(logDir, hex.EncodeToString(sum[:16])+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
}

func newSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
