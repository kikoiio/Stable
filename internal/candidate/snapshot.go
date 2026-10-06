package candidate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"stable/internal/platform/secfile"
	"strings"
	"sync"
	"time"
)

// FileSnapshot is the trusted record of one candidate's file state, bound
// to its owning project, session, candidate, and run.
type FileSnapshot struct {
	SnapshotID  string          `json:"snapshot_id"`
	ProjectID   string          `json:"project_id"`
	SessionID   string          `json:"session_id"`
	CandidateID string          `json:"candidate_id"`
	RunID       string          `json:"run_id,omitempty"`
	Label       string          `json:"label,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	Digest      string          `json:"digest"`
	Entries     []ManifestEntry `json:"entries"`
}

// SnapshotStore keeps content-addressed snapshot blobs and per-candidate
// manifests inside the project's .stable service directory. It never
// touches the formal project tree.
type SnapshotStore struct {
	root         string // <project>/.stable/candidate-snapshots
	serviceRoot  secfile.Root
	projectID    string
	maxBytes     int64
	maxManifests int
	credentials  []string
}

var snapshotLocks sync.Map

func snapshotLockFor(path string) *sync.Mutex {
	v, _ := snapshotLocks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// NewSnapshotStore opens (creating if needed) the snapshot area for one
// project. maxBytes bounds total blob storage per project; maxManifests
// bounds manifests per candidate. Credentials are redacted from snapshot
// metadata before it is written.
func NewSnapshotStore(projectRoot string, maxBytes int64, maxManifests int, credentials []string) (*SnapshotStore, error) {
	if maxBytes <= 0 {
		return nil, errors.New("snapshot byte quota is required")
	}
	if maxManifests <= 0 {
		return nil, errors.New("snapshot manifest quota is required")
	}
	project, err := secfile.OpenRoot(projectRoot)
	if err != nil {
		return nil, err
	}
	resolved := project.Path()
	stable := filepath.Join(resolved, ".stable")
	if st, e := os.Lstat(stable); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("project state directory must not be a symlink")
	}
	root := filepath.Join(resolved, ".stable", "candidate-snapshots")
	for _, dir := range []string{root, filepath.Join(root, "blobs"), filepath.Join(root, "manifests")} {
		if err = secfile.MkdirAllPrivate(dir, 0700); err != nil {
			return nil, err
		}
		if err = secfile.ChmodPrivate(dir, 0700); err != nil {
			return nil, err
		}
	}
	serviceRoot, err := secfile.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("snapshot service root is unsafe: %w", err)
	}
	if err := project.Revalidate(); err != nil {
		return nil, err
	}
	id := sha256.Sum256([]byte(resolved))
	return &SnapshotStore{
		root:         root,
		serviceRoot:  serviceRoot,
		projectID:    hex.EncodeToString(id[:]),
		maxBytes:     maxBytes,
		maxManifests: maxManifests,
		credentials:  credentials,
	}, nil
}

// Create snapshots the candidate directory: it records a trusted manifest,
// writes new content-addressed blobs, and stores the manifest. Quota
// failures return an error before any blob is written, so the caller can
// refuse the tool change instead of faking a checkpoint.
func (s *SnapshotStore) Create(sessionID, candidateID, runID, label, candidateRoot string) (FileSnapshot, error) {
	if sessionID == "" || candidateID == "" {
		return FileSnapshot{}, errors.New("snapshot requires session and candidate IDs")
	}
	label, err := s.redact(label)
	if err != nil {
		return FileSnapshot{}, err
	}
	entries, digest, err := BuildManifest(candidateRoot)
	if err != nil {
		return FileSnapshot{}, err
	}
	mu := snapshotLockFor(s.root)
	mu.Lock()
	defer mu.Unlock()
	if count, e := s.manifestCount(candidateID); e != nil {
		return FileSnapshot{}, e
	} else if count >= s.maxManifests {
		return FileSnapshot{}, fmt.Errorf("candidate %s reached the %d manifest limit", candidateID, s.maxManifests)
	}
	used, err := s.blobBytesLocked()
	if err != nil {
		return FileSnapshot{}, err
	}
	var pending int64
	for _, entry := range entries {
		if err := validateSnapshotDigest(entry.Digest); err != nil {
			return FileSnapshot{}, err
		}
		if entry.Size < 0 || entry.Size > s.maxBytes {
			return FileSnapshot{}, fmt.Errorf("snapshot entry %s exceeds the project byte budget", entry.Path)
		}
		if _, e := os.Lstat(s.blobPath(entry.Digest)); os.IsNotExist(e) {
			pending += entry.Size
		}
	}
	if used+pending > s.maxBytes {
		return FileSnapshot{}, fmt.Errorf("snapshot would exceed the %d byte project budget", s.maxBytes)
	}
	for _, entry := range entries {
		if err = s.writeBlobLocked(candidateRoot, entry); err != nil {
			return FileSnapshot{}, err
		}
	}
	snap := FileSnapshot{
		SnapshotID:  newSnapshotID(),
		ProjectID:   s.projectID,
		SessionID:   sessionID,
		CandidateID: candidateID,
		RunID:       runID,
		Label:       label,
		CreatedAt:   time.Now().UTC(),
		Digest:      digest,
		Entries:     entries,
	}
	if err = s.writeManifestLocked(snap); err != nil {
		return FileSnapshot{}, err
	}
	return snap, nil
}

// List returns the snapshots owned by one candidate, oldest first. Other
// candidates' snapshots are not visible.
func (s *SnapshotStore) List(candidateID string) ([]FileSnapshot, error) {
	if err := validateSnapshotComponent(candidateID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "manifests", candidateID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []FileSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.serviceRoot.Revalidate(); err != nil {
		return nil, err
	}
	out := []FileSnapshot{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		snap, err := s.readManifest(candidateID, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ValidateRestore loads the snapshot and verifies every referenced blob
// exists and still hashes to its content address. Corrupt or missing blobs
// refuse the restore.
func (s *SnapshotStore) ValidateRestore(candidateID, snapshotID string) (FileSnapshot, error) {
	snap, err := s.readManifest(candidateID, snapshotID)
	if err != nil {
		return FileSnapshot{}, err
	}
	for _, entry := range snap.Entries {
		if err := validateSnapshotDigest(entry.Digest); err != nil || entry.Size < 0 || entry.Size > s.maxBytes {
			return FileSnapshot{}, fmt.Errorf("snapshot blob %q has invalid metadata", entry.Digest)
		}
		blob, err := s.openServiceFile(filepath.Join("blobs", entry.Digest))
		if err != nil {
			return FileSnapshot{}, fmt.Errorf("snapshot blob %s unavailable: %w", entry.Digest[:12], err)
		}
		info, statErr := blob.Stat()
		if statErr != nil {
			_ = blob.Close()
			return FileSnapshot{}, statErr
		}
		if info.Size() != entry.Size {
			_ = blob.Close()
			return FileSnapshot{}, fmt.Errorf("snapshot blob %s size changed", entry.Digest[:12])
		}
		h := sha256.New()
		_, copyErr := io.CopyN(h, blob, entry.Size)
		if copyErr == nil {
			var extra [1]byte
			var extraErr error
			var extraN int
			extraN, extraErr = blob.Read(extra[:])
			if extraErr != nil && !errors.Is(extraErr, io.EOF) {
				copyErr = extraErr
			} else if extraN != 0 {
				copyErr = errors.New("snapshot blob has trailing data")
			}
		}
		closeErr := blob.Close()
		if copyErr != nil {
			return FileSnapshot{}, copyErr
		}
		if closeErr != nil {
			return FileSnapshot{}, closeErr
		}
		if hex.EncodeToString(h.Sum(nil)) != entry.Digest {
			return FileSnapshot{}, fmt.Errorf("snapshot blob %s is corrupt", entry.Digest[:12])
		}
	}
	if err := s.serviceRoot.Revalidate(); err != nil {
		return FileSnapshot{}, err
	}
	return snap, nil
}

// Materialize rebuilds a validated snapshot into a staging directory and
// verifies the result against the snapshot digest. The caller performs the
// atomic directory exchange; the formal project tree is never a target.
func (s *SnapshotStore) Materialize(snap FileSnapshot, stagingDir string) error {
	if snap.ProjectID != s.projectID {
		return errors.New("snapshot belongs to a different project")
	}
	stagingDir, err := filepath.Abs(stagingDir)
	if err != nil {
		return err
	}
	st, err := os.Lstat(stagingDir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("staging target must be a real directory")
	}
	for _, entry := range snap.Entries {
		rel, err := CleanRelative(entry.Path)
		if err != nil {
			return fmt.Errorf("snapshot entry escapes staging: %w", err)
		}
		dst := filepath.Join(stagingDir, rel)
		if err = secfile.MkdirAllPrivate(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err = copyBlob(s.blobPath(entry.Digest), dst, os.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	_, digest, err := BuildManifest(stagingDir)
	if err != nil {
		return err
	}
	if digest != snap.Digest {
		return fmt.Errorf("restored staging digest does not match snapshot %s", snap.SnapshotID)
	}
	return nil
}

func (s *SnapshotStore) blobPath(digest string) string {
	return filepath.Join(s.root, "blobs", digest)
}

func (s *SnapshotStore) openServiceFile(rel string) (*os.File, error) {
	clean, err := CleanRelative(rel)
	if err != nil {
		return nil, err
	}
	f, err := s.serviceRoot.Open(clean)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, secfile.ErrUnsafePath
	}
	return f, nil
}

func validateSnapshotComponent(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\:\x00") {
		return ErrUnsafePath
	}
	return nil
}

func validateSnapshotDigest(digest string) error {
	if len(digest) != sha256.Size*2 {
		return ErrUnsafePath
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ErrUnsafePath
	}
	return nil
}

func (s *SnapshotStore) blobBytesLocked() (int64, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "blobs"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (s *SnapshotStore) manifestCount(candidateID string) (int, error) {
	if err := validateSnapshotComponent(candidateID); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "manifests", candidateID))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count, nil
}

func (s *SnapshotStore) writeBlobLocked(candidateRoot string, entry ManifestEntry) error {
	if err := validateSnapshotDigest(entry.Digest); err != nil {
		return err
	}
	if _, err := CleanRelative(entry.Path); err != nil {
		return err
	}
	path := s.blobPath(entry.Digest)
	if _, err := os.Lstat(path); err == nil {
		return nil // same content already stored; never double-counted
	}
	in, err := secureOpen(candidateRoot, entry.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(s.root, ".blob-*")
	if err != nil {
		return err
	}
	if err = secfile.ChmodPrivate(tmp.Name(), 0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), in)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp.Name())
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	if hex.EncodeToString(h.Sum(nil)) != entry.Digest {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("candidate file %s changed while snapshotting", entry.Path)
	}
	if err = secfile.ChmodPrivate(tmp.Name(), 0600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *SnapshotStore) writeManifestLocked(snap FileSnapshot) error {
	dir := filepath.Join(s.root, "manifests", snap.CandidateID)
	if err := secfile.MkdirAllPrivate(dir, 0700); err != nil {
		return err
	}
	if err := secfile.ChmodPrivate(dir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err = secfile.ChmodPrivate(tmp.Name(), 0600); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, snap.SnapshotID+".json"))
}

func (s *SnapshotStore) readManifest(candidateID, snapshotID string) (FileSnapshot, error) {
	if err := validateSnapshotComponent(candidateID); err != nil {
		return FileSnapshot{}, err
	}
	if err := validateSnapshotComponent(snapshotID); err != nil {
		return FileSnapshot{}, err
	}
	manifestRel := filepath.Join("manifests", candidateID, snapshotID+".json")
	manifestFile, err := s.openServiceFile(manifestRel)
	if err != nil {
		return FileSnapshot{}, err
	}
	info, err := manifestFile.Stat()
	if err != nil {
		_ = manifestFile.Close()
		return FileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		_ = manifestFile.Close()
		return FileSnapshot{}, ErrUnsafePath
	}
	raw, err := io.ReadAll(manifestFile)
	closeErr := manifestFile.Close()
	if err != nil {
		return FileSnapshot{}, err
	}
	if closeErr != nil {
		return FileSnapshot{}, closeErr
	}
	if err := s.serviceRoot.Revalidate(); err != nil {
		return FileSnapshot{}, err
	}
	var snap FileSnapshot
	if err = json.Unmarshal(raw, &snap); err != nil {
		return FileSnapshot{}, fmt.Errorf("snapshot manifest unreadable: %w", err)
	}
	if snap.ProjectID != s.projectID || snap.CandidateID != candidateID || snap.SnapshotID != snapshotID || snap.Digest == "" {
		return FileSnapshot{}, errors.New("snapshot ownership does not match this project")
	}
	return snap, nil
}

// redact applies the same credential replacement rule as the session log
// path: configured credentials never reach stored metadata, and a credential
// too short to replace safely refuses the write instead of saving plaintext.
func (s *SnapshotStore) redact(text string) (string, error) {
	for _, credential := range s.credentials {
		if credential == "" {
			continue
		}
		if len(credential) < 8 {
			if strings.Contains(text, credential) {
				return "", errors.New("credential too short to redact safely")
			}
			continue
		}
		text = strings.ReplaceAll(text, credential, "[credential redacted]")
	}
	return text, nil
}

func newSnapshotID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func copyBlob(blobPath, dst string, mode os.FileMode) error {
	in, err := secfile.OpenNoFollow(blobPath)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return secfile.ErrUnsafePath
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}
