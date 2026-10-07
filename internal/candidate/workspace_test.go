package candidate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManifestPath(t *testing.T) {
	for _, p := range []string{"../secret", "/tmp/file", "a/../../secret", "."} {
		if _, err := CleanRelative(p); err == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	if got, err := CleanRelative("sub/file.kicad_sch"); err != nil || got != "sub/file.kicad_sch" {
		t.Fatalf("clean relative: %q %v", got, err)
	}
}

func TestManifestAndCandidateCopy(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(filepath.Join(formal, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "nested", "board.kicad_sch"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, digest, err := BuildManifest(formal)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || digest == "" {
		t.Fatalf("manifest = %#v, %q", entries, digest)
	}
	c, err := CreateCandidate("cand-1", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if c.BaselineDigest != digest {
		t.Fatal("candidate baseline digest mismatch")
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "nested", "board.kicad_sch"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(formal, "nested", "board.kicad_sch"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("formal project changed: %q", got)
	}
	if _, _, err = BuildManifest(filepath.Join(c.CandidateRoot)); err != nil {
		t.Fatal(err)
	}
}

func TestProjectV2ManifestExcludesRootProtectedMetadata(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".git/config", ".stable/session.json", ".mewcode/settings.json", "src/main.go"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, digest, err := BuildManifestForPolicy(root, ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != filepath.Join("src", "main.go") {
		t.Fatalf("project-v2 entries = %+v", entries)
	}
	if err = os.WriteFile(filepath.Join(root, ".git", "config"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	_, after, err := BuildManifestForPolicy(root, ManifestPolicyProject)
	if err != nil || after != digest {
		t.Fatalf("protected metadata changed project digest: before=%s after=%s err=%v", digest, after, err)
	}
	if _, _, err = BuildManifestForPolicy(root, "unknown"); err == nil {
		t.Fatal("unknown manifest policy was accepted")
	}
}

func TestProjectV2RejectsNestedGitAndCandidateMetadataInjection(t *testing.T) {
	formal := t.TempDir()
	if err := os.MkdirAll(filepath.Join(formal, "vendor", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildManifestForPolicy(formal, ManifestPolicyProject); err == nil {
		t.Fatal("nested .git metadata was accepted")
	}

	formal = t.TempDir()
	if err := os.Mkdir(filepath.Join(formal, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	candidateRoot := t.TempDir()
	if err := ValidateProtectedMetadata(formal, candidateRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, ".git"), []byte("injected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProtectedMetadata(formal, candidateRoot); err == nil {
		t.Fatal("candidate metadata injection was accepted")
	}
}

func TestProtectedMetadataIdentityAndPartialRestore(t *testing.T) {
	formal := t.TempDir()
	spent := t.TempDir()
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		if err := os.Mkdir(filepath.Join(formal, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(formal, name, "state"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := CaptureProtectedMetadata(formal)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		if err = os.Rename(filepath.Join(formal, name), filepath.Join(spent, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Rename(filepath.Join(spent, ".git"), filepath.Join(formal, ".git")); err != nil {
		t.Fatal(err)
	}
	if err = RestoreProtectedMetadataFacts(formal, spent, facts); err != nil {
		t.Fatal(err)
	}
	if err = RestoreProtectedMetadataFacts(formal, spent, facts); err != nil {
		t.Fatalf("second metadata recovery was not idempotent: %v", err)
	}
	if err = os.Rename(filepath.Join(formal, ".git"), filepath.Join(spent, ".git")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(formal, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = RestoreProtectedMetadataFacts(formal, spent, facts); err == nil {
		t.Fatal("identity mismatch was not blocked")
	}
}

func TestCreateCandidateProjectV2OmitsProtectedMetadata(t *testing.T) {
	formal := t.TempDir()
	for _, name := range []string{".git/config", ".stable/state", ".mewcode/settings"} {
		path := filepath.Join(formal, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := CreateCandidateForPolicy("candidate-v2", formal, t.TempDir(), ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if c.ManifestPolicy != ManifestPolicyProject {
		t.Fatalf("manifest policy = %q", c.ManifestPolicy)
	}
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		if _, err = os.Lstat(filepath.Join(c.CandidateRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("candidate contains %s: %v", name, err)
		}
	}
	if _, err = os.Stat(filepath.Join(c.CandidateRoot, "source.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestProjectV2AcceptanceRestoresProtectedMetadata(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	candidateParent := filepath.Join(root, "candidates")
	for _, name := range []string{".git/config", ".stable/session.json", ".mewcode/settings.json"} {
		path := filepath.Join(formal, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("original:"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "source.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := CreateCandidateForPolicy("candidate-accept-v2", formal, candidateParent, ManifestPolicyProject)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "source.txt"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = FreezeCandidate(c, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	review, err := BuildReview(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "reviewed"
	decision := AcceptanceDecision{ID: "accept-v2", UserID: "test-user", CandidateID: c.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: AcceptNormal}
	if _, err = AcceptCandidate(context.Background(), c, review, decision, "session-test", "", &memoryAcceptance{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git/config", ".stable/session.json", ".mewcode/settings.json"} {
		data, readErr := os.ReadFile(filepath.Join(formal, name))
		if readErr != nil || string(data) != "original:"+name {
			t.Fatalf("protected metadata %s changed: data=%q err=%v", name, data, readErr)
		}
	}
	if data, readErr := os.ReadFile(filepath.Join(formal, "source.txt")); readErr != nil || string(data) != "after" {
		t.Fatalf("project content was not accepted: data=%q err=%v", data, readErr)
	}
}

func TestManifestRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildManifest(root); err == nil {
		t.Fatal("manifest accepted symlink")
	}
}

type fixedChecker struct{ finding Finding }

type memoryAcceptance struct {
	saved    bool
	receipt  *Receipt
	phase    string
	metadata []ProtectedMetadataFact
}

func (m *memoryAcceptance) CheckAcceptance(_ context.Context, _ AcceptanceDecision) (bool, Receipt, bool, error) {
	if !m.saved {
		return false, Receipt{}, false, nil
	}
	if m.receipt != nil {
		return true, *m.receipt, true, nil
	}
	return true, Receipt{}, false, nil
}

func (m *memoryAcceptance) SaveAcceptanceDecision(context.Context, AcceptanceDecision) (bool, error) {
	if m.saved {
		return false, nil
	}
	m.saved = true
	m.phase = "prepared"
	return true, nil
}
func (m *memoryAcceptance) SaveProtectedMetadata(_ context.Context, _ string, facts []ProtectedMetadataFact) error {
	m.metadata = append([]ProtectedMetadataFact(nil), facts...)
	return nil
}
func (m *memoryAcceptance) LoadProtectedMetadata(context.Context, string) ([]ProtectedMetadataFact, error) {
	return append([]ProtectedMetadataFact(nil), m.metadata...), nil
}
func (m *memoryAcceptance) FindAcceptanceReceipt(_ context.Context, _ string) (Receipt, bool, error) {
	if m.receipt == nil {
		return Receipt{}, false, nil
	}
	return *m.receipt, true, nil
}
func (m *memoryAcceptance) SetAcceptancePhase(_ context.Context, _ string, from, to, _ string) error {
	if m.phase != from {
		return errors.New("phase mismatch")
	}
	m.phase = to
	return nil
}
func (m *memoryAcceptance) FinalizeAcceptance(_ context.Context, _ AcceptanceDecision, r Receipt, _, _ string) error {
	m.receipt = &r
	m.phase = "finalized"
	return nil
}

func (f fixedChecker) Check(context.Context, Candidate) (Finding, error) { return f.finding, nil }

func TestReviewDiffAndFindings(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{"edit.txt": "before\n", "remove.txt": "removed\n"} {
		if err := os.WriteFile(filepath.Join(formal, path), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := CreateCandidate("review-1", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "edit.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(c.CandidateRoot, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "new.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = FreezeCandidate(c, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checker := fixedChecker{finding: Finding{ID: "erc", Checker: "erc", Version: "1", Result: FindingUnavailable, Reason: "kicad-cli missing"}}
	r, err := BuildReview(context.Background(), c, []Checker{checker})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 3 || r.Findings[0].Result != FindingUnavailable || r.Digest == "" {
		t.Fatalf("review incomplete: %+v", r)
	}
	statuses := map[string]bool{}
	for _, change := range r.Changes {
		statuses[change.Status] = true
	}
	if !statuses["added"] || !statuses["modified"] || !statuses["deleted"] {
		t.Fatalf("change statuses: %#v", statuses)
	}
}

func TestAcceptGuardsAndAtomicExchange(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := CreateCandidate("accept-1", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = FreezeCandidate(c, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finding := Finding{ID: "erc", Checker: "erc", Result: FindingFail, Reason: "one violation"}
	review, err := BuildReview(context.Background(), c, []Checker{fixedChecker{finding: finding}})
	if err != nil {
		t.Fatal(err)
	}
	c.Status = "reviewed"
	decision := AcceptanceDecision{ID: "decision-1", UserID: "user", CandidateID: c.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: AcceptNormal}
	store := &memoryAcceptance{}
	if _, err = AcceptCandidate(context.Background(), c, review, decision, "goal", "action", store, time.Time{}); err == nil {
		t.Fatal("normal acceptance ignored failing finding")
	}
	decision.Mode = AcceptForce
	if _, err = AcceptCandidate(context.Background(), c, review, decision, "goal", "action", store, time.Time{}); err == nil {
		t.Fatal("force acceptance skipped per-finding confirmation")
	}
	decision.ConfirmedFindings = []string{"erc"}
	receipt, err := AcceptCandidate(context.Background(), c, review, decision, "goal", "action", store, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != "receipt-decision-1" || receipt.FormalDigest != review.CandidateDigest {
		t.Fatalf("receipt=%+v", receipt)
	}
	got, err := os.ReadFile(filepath.Join(formal, "board"))
	if err != nil || string(got) != "new" {
		t.Fatalf("formal project was not exchanged: %q %v", got, err)
	}
}

func TestAcceptRejectsStalePreview(t *testing.T) {
	root := t.TempDir()
	formal := filepath.Join(root, "formal")
	parent := filepath.Join(root, "candidates")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := CreateCandidate("stale", formal, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.CandidateRoot, "board"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = FreezeCandidate(c, nil, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	review, err := BuildReview(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(formal, "board"), []byte("external"), 0600); err != nil {
		t.Fatal(err)
	}
	d := AcceptanceDecision{ID: "d", UserID: "u", CandidateID: c.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: AcceptNormal}
	if _, err = AcceptCandidate(context.Background(), c, review, d, "goal", "action", &memoryAcceptance{}, time.Time{}); err == nil {
		t.Fatal("stale preview accepted")
	}
}
