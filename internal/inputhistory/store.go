// Package inputhistory keeps a private, bounded, per-project history of user
// inputs in .stable/input-history.jsonl. The history is stored separately
// from the session transcript and never feeds context projections.
package inputhistory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// MaxEntries bounds the history; the oldest entries are dropped first.
const MaxEntries = 200

var locks sync.Map

func lockFor(path string) *sync.Mutex {
	v, _ := locks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Entry is one stored input with the time it was recorded.
type Entry struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Store appends and browses the input history of one project.
type Store struct {
	path        string
	credentials []string
}

// Open prepares the project state directory and returns the history store.
// Credentials are redacted from every appended input.
func Open(root string, credentials ...string) (*Store, error) {
	path, err := historyPath(root)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, credentials: credentials}, nil
}

// Append redacts and stores one input. Blank inputs are ignored, and an
// input equal to the most recent entry is not stored again; both return a
// zero Entry and no error. When the history exceeds MaxEntries the oldest
// entries are dropped. A corrupt history or a credential that cannot be
// redacted safely fails the append without writing plaintext.
func (s *Store) Append(text string) (Entry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Entry{}, nil
	}
	text, err := redact(text, s.credentials)
	if err != nil {
		return Entry{}, err
	}
	mu := lockFor(s.path)
	mu.Lock()
	defer mu.Unlock()
	entries, err := readEntries(s.path)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) > 0 && entries[len(entries)-1].Text == text {
		return Entry{}, nil
	}
	entry := Entry{Text: text, At: time.Now().UTC()}
	if len(entries) >= MaxEntries {
		kept := append(entries[len(entries)-MaxEntries+1:], entry)
		return entry, rewriteFile(s.path, kept)
	}
	return entry, appendLine(s.path, entry)
}

// List returns the stored entries in chronological order, oldest first.
func (s *Store) List() ([]Entry, error) {
	mu := lockFor(s.path)
	mu.Lock()
	defer mu.Unlock()
	return readEntries(s.path)
}

// Cursor returns a bounded browser over a snapshot of the history,
// positioned past the newest entry so the first Prev yields it.
func (s *Store) Cursor() (*Cursor, error) {
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	return &Cursor{entries: entries, pos: len(entries)}, nil
}

// Cursor walks a history snapshot; Prev steps toward older entries and Next
// steps back toward newer ones. Both report false at their end, and Next
// reports false once it returns to the fresh-input position.
type Cursor struct {
	entries []Entry
	pos     int
}

// Prev returns the next older entry, or false at the oldest one.
func (c *Cursor) Prev() (Entry, bool) {
	if c.pos == 0 {
		return Entry{}, false
	}
	c.pos--
	return c.entries[c.pos], true
}

// Next returns the next newer entry, or false once past the newest one.
func (c *Cursor) Next() (Entry, bool) {
	if c.pos >= len(c.entries)-1 {
		c.pos = len(c.entries)
		return Entry{}, false
	}
	c.pos++
	return c.entries[c.pos], true
}

func readEntries(path string) ([]Entry, error) {
	entries := []Entry{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err = f.ReadAt(last, info.Size()-1); err != nil {
			return nil, err
		}
		if last[0] != '\n' {
			return nil, errors.New("input history has incomplete final line")
		}
	}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 8<<20)
	line := 0
	for s.Scan() {
		line++
		var e Entry
		if err = json.Unmarshal(s.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("input history corrupt at line %d: %w", line, err)
		}
		if strings.TrimSpace(e.Text) == "" || e.At.IsZero() {
			return nil, fmt.Errorf("input history has invalid entry at line %d", line)
		}
		entries = append(entries, e)
	}
	if err = s.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func appendLine(path string, entry Entry) error {
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	oldSize := info.Size()
	n, err := f.Write(b)
	if err != nil {
		_ = f.Truncate(oldSize)
		return err
	}
	if n != len(b) {
		_ = f.Truncate(oldSize)
		return io.ErrShortWrite
	}
	return f.Sync()
}

func rewriteFile(path string, entries []Entry) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		b, err := json.Marshal(entry)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
		if _, err = f.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
