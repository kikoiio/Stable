package candidate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

type FindingResult string

const (
	FindingPass        FindingResult = "pass"
	FindingFail        FindingResult = "fail"
	FindingUnavailable FindingResult = "unavailable"
)

type Finding struct {
	ID      string        `json:"id"`
	Checker string        `json:"checker"`
	Version string        `json:"version"`
	Result  FindingResult `json:"result"`
	Files   []string      `json:"files,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

type FileChange struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	BeforeMode   uint32 `json:"before_mode,omitempty"`
	AfterMode    uint32 `json:"after_mode,omitempty"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest,omitempty"`
	BeforeBase64 string `json:"before_base64,omitempty"`
	AfterBase64  string `json:"after_base64,omitempty"`
	TextDiff     string `json:"text_diff,omitempty"`
}

type Review struct {
	ID              string       `json:"id"`
	CandidateID     string       `json:"candidate_id"`
	FormalDigest    string       `json:"formal_digest"`
	CandidateDigest string       `json:"candidate_digest"`
	Changes         []FileChange `json:"changes"`
	Findings        []Finding    `json:"findings"`
	Digest          string       `json:"digest"`
}

type Checker interface {
	Check(context.Context, Candidate) (Finding, error)
}

func BuildReview(ctx context.Context, c Candidate, checkers []Checker) (Review, error) {
	if c.Status != "frozen" && c.Status != "reviewed" {
		return Review{}, fmt.Errorf("candidate %s is not frozen", c.ID)
	}
	formal, formalDigest, err := BuildManifest(c.FormalRoot)
	if err != nil {
		return Review{}, err
	}
	candidate, candidateDigest, err := BuildManifest(c.CandidateRoot)
	if err != nil {
		return Review{}, err
	}
	changes, err := DiffManifests(c.FormalRoot, c.CandidateRoot, formal, candidate)
	if err != nil {
		return Review{}, err
	}
	review := Review{ID: "review-" + c.ID, CandidateID: c.ID, FormalDigest: formalDigest, CandidateDigest: candidateDigest, Changes: changes, Findings: []Finding{}}
	changePaths := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		changePaths[change.Path] = struct{}{}
	}
	seenFindings := map[string]struct{}{}
	for _, checker := range checkers {
		finding, checkErr := checker.Check(ctx, c)
		if checkErr != nil {
			finding = Finding{ID: "checker-" + fmt.Sprint(len(review.Findings)+1), Checker: fmt.Sprintf("%T", checker), Result: FindingUnavailable, Reason: checkErr.Error()}
		}
		if finding.ID == "" || finding.Checker == "" {
			return Review{}, errors.New("checker returned incomplete finding")
		}
		if _, duplicate := seenFindings[finding.ID]; duplicate {
			return Review{}, fmt.Errorf("checker returned duplicate finding ID %q", finding.ID)
		}
		seenFindings[finding.ID] = struct{}{}
		if finding.Result != FindingPass && finding.Result != FindingFail && finding.Result != FindingUnavailable {
			return Review{}, fmt.Errorf("checker %s returned invalid result", finding.Checker)
		}
		finding.Files = sortedUnique(finding.Files)
		for _, path := range finding.Files {
			if _, err := CleanRelative(path); err != nil {
				return Review{}, fmt.Errorf("checker %s returned unsafe file path: %w", finding.Checker, err)
			}
			if _, exists := changePaths[path]; !exists {
				return Review{}, fmt.Errorf("checker %s returned file outside the candidate diff: %s", finding.Checker, path)
			}
		}
		review.Findings = append(review.Findings, finding)
	}
	_, formalAfter, err := BuildManifest(c.FormalRoot)
	if err != nil {
		return Review{}, err
	}
	_, candidateAfter, err := BuildManifest(c.CandidateRoot)
	if err != nil {
		return Review{}, err
	}
	if formalAfter != review.FormalDigest || candidateAfter != review.CandidateDigest {
		return Review{}, errors.New("project changed while candidate checks were running; rebuild the review")
	}
	review.Digest, err = ComputeReviewDigest(review)
	if err != nil {
		return Review{}, err
	}
	return review, nil
}

func ComputeReviewDigest(review Review) (string, error) {
	encoded, err := json.Marshal(struct {
		Candidate string
		Formal    string
		Changes   []FileChange
		Findings  []Finding
	}{review.CandidateDigest, review.FormalDigest, review.Changes, review.Findings})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func DiffManifests(formalRoot, candidateRoot string, formal, candidate []ManifestEntry) ([]FileChange, error) {
	a := map[string]ManifestEntry{}
	b := map[string]ManifestEntry{}
	for _, e := range formal {
		a[e.Path] = e
	}
	for _, e := range candidate {
		b[e.Path] = e
	}
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	changes := []FileChange{}
	for _, p := range ordered {
		old, hadOld := a[p]
		newEntry, hasNew := b[p]
		if hadOld && hasNew && old.Digest == newEntry.Digest && old.Mode == newEntry.Mode {
			continue
		}
		change := FileChange{Path: p, Status: "modified"}
		if !hadOld {
			change.Status = "added"
		}
		if !hasNew {
			change.Status = "deleted"
		}
		if hadOld {
			change.BeforeDigest = old.Digest
			change.BeforeMode = old.Mode
			data, err := readEntry(formalRoot, old)
			if err != nil {
				return nil, err
			}
			change.BeforeBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if hasNew {
			change.AfterDigest = newEntry.Digest
			change.AfterMode = newEntry.Mode
			data, err := readEntry(candidateRoot, newEntry)
			if err != nil {
				return nil, err
			}
			change.AfterBase64 = base64.StdEncoding.EncodeToString(data)
		}
		if hadOld && hasNew {
			before, _ := base64.StdEncoding.DecodeString(change.BeforeBase64)
			after, _ := base64.StdEncoding.DecodeString(change.AfterBase64)
			if utf8.Valid(before) && utf8.Valid(after) {
				change.TextDiff = TextDiff(p, string(before), string(after))
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func readEntry(root string, entry ManifestEntry) ([]byte, error) {
	f, err := secureOpen(root, entry.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if uint32(info.Mode().Perm()) != entry.Mode || info.Size() != entry.Size {
		return nil, errors.New("project metadata changed while building review")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != entry.Digest {
		return nil, errors.New("project changed while building review")
	}
	return data, nil
}

func TextDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	old := strings.Split(before, "\n")
	next := strings.Split(after, "\n")
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(old), len(next))
	for _, line := range old {
		out.WriteByte('-')
		out.WriteString(line)
		out.WriteByte('\n')
	}
	for _, line := range next {
		out.WriteByte('+')
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func sortedUnique(in []string) []string {
	set := map[string]bool{}
	for _, x := range in {
		set[x] = true
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
