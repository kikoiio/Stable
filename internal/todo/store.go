package todo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"stable/internal/platform/secfile"
	"strings"

	"stable/internal/redact"
)

// DirName is the project-relative directory holding per-session task lists.
const DirName = ".stable/tasks"

// Store persists one session's tasks as private JSON and strips configured
// credentials from every persisted text field before writing.
type Store struct {
	path        string
	credentials []string
}

// NewStore returns the store of one session inside the project. A session id
// that is empty or contains path separators or ".." is rejected so the
// returned path always stays inside the tasks directory.
func NewStore(projectRoot, sessionID string) (*Store, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, errors.New("project root is required")
	}
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	dir := filepath.Join(projectRoot, DirName)
	path := filepath.Join(dir, sessionID+".json")
	if filepath.Dir(path) != dir {
		return nil, errors.New("tasks path escapes tasks directory")
	}
	return &Store{path: path}, nil
}

// setCredentials replaces the secrets stripped from persisted text fields.
func (s *Store) setCredentials(credentials []string) {
	s.credentials = append(s.credentials[:0:0], credentials...)
}

// Load returns the persisted tasks. A missing file is an empty list; a
// symlinked or unreadable file is an error so callers never fall back to
// silently losing tasks.
func (s *Store) Load() ([]Task, error) {
	st, err := os.Lstat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("tasks file must not be a symlink")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("decode tasks file: %w", err)
	}
	return tasks, nil
}

// Save persists the tasks and returns the redacted snapshot it wrote. Every
// text field is stripped of configured credentials first; a redaction
// failure refuses the whole write, so plaintext never reaches the disk.
func (s *Store) Save(tasks []Task) ([]Task, error) {
	redacted, err := s.redactTasks(tasks)
	if err != nil {
		return nil, err
	}
	if err := ensureTasksDir(s.path); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(s.path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("tasks file must not be a symlink")
		}
		if !st.Mode().IsRegular() {
			return nil, errors.New("tasks path must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	data, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := secfile.OpenFilePrivate(s.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err = secfile.ChmodPrivate(f.Name(), 0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return redacted, nil
}

// redactTasks copies tasks with every persisted text field stripped of the
// configured credentials. Dependencies are ids and never carry secrets, so
// only free-text fields are passed through redact.
func (s *Store) redactTasks(tasks []Task) ([]Task, error) {
	redacted := make([]Task, len(tasks))
	for i, task := range tasks {
		fields := []string{task.Subject, task.Description, task.ActiveForm, task.Owner}
		for j := range fields {
			text, err := redact.Redact(fields[j], s.credentials)
			if err != nil {
				return nil, fmt.Errorf("redact task %s: %w", task.ID, err)
			}
			fields[j] = text
		}
		redacted[i] = Task{
			ID:          task.ID,
			Subject:     fields[0],
			Description: fields[1],
			ActiveForm:  fields[2],
			Status:      task.Status,
			Owner:       fields[3],
			Blocks:      task.Blocks,
			BlockedBy:   task.BlockedBy,
			Metadata:    make(map[string]string, len(task.Metadata)),
		}
		for key, value := range task.Metadata {
			text, err := redact.Redact(value, s.credentials)
			if err != nil {
				return nil, fmt.Errorf("redact task %s: %w", task.ID, err)
			}
			redacted[i].Metadata[key] = text
		}
	}
	return redacted, nil
}

// ensureTasksDir creates the state and tasks directories with private
// permissions and refuses symlinked directories on the way to the tasks
// file. The explicit Chmods defeat permissive umasks.
func ensureTasksDir(path string) error {
	dir := filepath.Dir(path)
	stable := filepath.Dir(dir)
	if st, err := os.Lstat(stable); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must not be a symlink")
	}
	if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("tasks directory must not be a symlink")
	}
	if err := secfile.MkdirAllPrivate(dir, 0700); err != nil {
		return err
	}
	if err := secfile.ChmodPrivate(stable, 0700); err != nil {
		return err
	}
	if err := secfile.ChmodPrivate(dir, 0700); err != nil {
		return err
	}
	return nil
}

// validateSessionID rejects ids that could move the tasks path outside the
// tasks directory.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if strings.ContainsAny(sessionID, `/\`) {
		return errors.New("session id must not contain path separators")
	}
	if strings.Contains(sessionID, "..") {
		return errors.New(`session id must not contain ".."`)
	}
	return nil
}

// randomID generates task ids in the same "prefix-<hex>" shape used across
// the project (goalrun.RandomID style); the helper there is unexported, so
// the format is reproduced locally instead of being reimplemented differently.
func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generate task id: %v", err))
	}
	return "task-" + hex.EncodeToString(b)
}
