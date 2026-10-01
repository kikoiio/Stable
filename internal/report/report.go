package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"stable/internal/core"
)

type Status struct {
	Snapshot         core.GoalSnapshot    `json:"snapshot"`
	ActualArtifactID string               `json:"actual_artifact_id"`
	Verified         bool                 `json:"verified"`
	Unverified       []string             `json:"unverified"`
	Evidence         []NormalizedEvidence `json:"evidence"`
}

// NormalizedEvidence is the status/export view of one evidence row. Fields
// missing on legacy rows (criteria revision, provenance) are rendered as the
// literal string "unknown" instead of being inferred.
type NormalizedEvidence struct {
	ID                string `json:"id"`
	CriterionID       string `json:"criterion_id"`
	Kind              string `json:"kind"`
	Result            string `json:"result"`
	ReportPath        string `json:"report_path,omitempty"`
	CriteriaRevision  string `json:"criteria_revision"` // revision number or "unknown"
	CheckerID         string `json:"checker_id"`
	CheckerVersion    string `json:"checker_version"`
	SourceLevel       string `json:"source_level"`
	InvalidatedReason string `json:"invalidated_reason,omitempty"`
	Current           bool   `json:"current"`
}

func normalizeEvidence(snapshot core.GoalSnapshot, artifactID string, e core.Evidence) NormalizedEvidence {
	out := NormalizedEvidence{
		ID:                e.ID,
		CriterionID:       e.CriterionID,
		Kind:              e.Kind,
		Result:            e.Result,
		ReportPath:        e.ReportPath,
		CriteriaRevision:  "unknown",
		CheckerID:         "unknown",
		CheckerVersion:    "unknown",
		SourceLevel:       "unknown",
		InvalidatedReason: e.InvalidatedReason,
		Current:           core.EvidenceCurrent(snapshot.Goal, artifactID, e),
	}
	if e.CriteriaRevision != nil {
		out.CriteriaRevision = fmt.Sprintf("%d", *e.CriteriaRevision)
	}
	if p := e.Provenance; p != nil {
		if p.CheckerID != "" {
			out.CheckerID = p.CheckerID
		}
		if p.CheckerVersion != "" {
			out.CheckerVersion = p.CheckerVersion
		}
		if p.SourceLevel != "" {
			out.SourceLevel = p.SourceLevel
		}
	}
	return out
}

func BuildStatus(snapshot core.GoalSnapshot) (Status, error) {
	out := Status{Snapshot: snapshot, Unverified: []string{}}
	f, err := os.Open(snapshot.Goal.ArtifactPath)
	if err != nil {
		return out, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return out, err
	}
	out.ActualArtifactID = hex.EncodeToString(h.Sum(nil))
	if out.ActualArtifactID != snapshot.Goal.CurrentArtifactID {
		out.Unverified = append(out.Unverified, "design differs from recorded artifact digest")
	}
	for _, criterion := range snapshot.Goal.Criteria {
		current := currentEvidenceFor(snapshot, criterion.ID, out.ActualArtifactID)
		if current == nil {
			out.Unverified = append(out.Unverified, "no current evidence for criterion "+criterion.ID)
			continue
		}
		if criterion.Kind == core.CriterionKindERCClean {
			var payload struct {
				MaxViolations int `json:"max_violations"`
			}
			if err := json.Unmarshal(criterion.Payload, &payload); err != nil {
				out.Unverified = append(out.Unverified, "criterion "+criterion.ID+": unreadable max_violations")
				continue
			}
			violations, err := reportViolations(current.ReportPath)
			if err != nil {
				out.Unverified = append(out.Unverified, "criterion "+criterion.ID+": ERC report unreadable")
				continue
			}
			if violations > payload.MaxViolations {
				out.Unverified = append(out.Unverified, fmt.Sprintf("criterion %s: ERC reports %d violations, threshold %d", criterion.ID, violations, payload.MaxViolations))
			}
		}
	}
	if snapshot.Goal.Status != core.GoalVerified {
		out.Unverified = append(out.Unverified, "goal status is "+string(snapshot.Goal.Status))
	}
	for _, e := range snapshot.Evidence {
		out.Evidence = append(out.Evidence, normalizeEvidence(snapshot, out.ActualArtifactID, e))
	}
	out.Verified = len(out.Unverified) == 0
	return out, nil
}

// currentEvidenceFor returns the newest current evidence supporting the given
// criterion, as admitted by the generic EvidenceCurrent filter.
func currentEvidenceFor(snapshot core.GoalSnapshot, criterionID, artifactID string) *core.Evidence {
	for i := range snapshot.Evidence {
		e := snapshot.Evidence[i]
		if e.CriterionID == criterionID && core.EvidenceCurrent(snapshot.Goal, artifactID, e) {
			return &snapshot.Evidence[i]
		}
	}
	return nil
}

// reportViolations counts ERC violations across all sheets of an independent
// JSON report. Unreadable or malformed reports are an error, not zero.
func reportViolations(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var report struct {
		Sheets []struct {
			Violations []json.RawMessage `json:"violations"`
		} `json:"sheets"`
	}
	if json.Unmarshal(data, &report) != nil || report.Sheets == nil {
		return 0, errors.New("invalid ERC report")
	}
	total := 0
	for _, sheet := range report.Sheets {
		total += len(sheet.Violations)
	}
	return total, nil
}

func Export(ctx context.Context, snapshot core.GoalSnapshot, outDir string) (Status, error) {
	_ = ctx
	status, err := BuildStatus(snapshot)
	if err != nil {
		return status, err
	}
	if outDir == "" {
		return status, errors.New("export directory required")
	}
	if err = os.MkdirAll(outDir, 0755); err != nil {
		return status, err
	}
	copyFile := func(src, dst string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	if err = copyFile(snapshot.Goal.ArtifactPath, filepath.Join(outDir, filepath.Base(snapshot.Goal.ArtifactPath))); err != nil {
		return status, err
	}
	// Only the ERC report that supports the current criteria and artifact is
	// delivered; superseded or invalidated reports are archived below.
	for _, criterion := range snapshot.Goal.Criteria {
		if criterion.Kind != core.CriterionKindERCClean {
			continue
		}
		current := currentEvidenceFor(snapshot, criterion.ID, status.ActualArtifactID)
		if current == nil || current.Kind != "kicad.erc" {
			continue
		}
		var payload struct {
			MaxViolations int `json:"max_violations"`
		}
		if err := json.Unmarshal(criterion.Payload, &payload); err != nil {
			continue
		}
		violations, err := reportViolations(current.ReportPath)
		if err != nil || violations > payload.MaxViolations {
			continue
		}
		if err = copyFile(current.ReportPath, filepath.Join(outDir, "erc.json")); err != nil {
			return status, err
		}
		break
	}
	// Archive readable reports of non-current evidence, keeping the original
	// result and the invalidation reason in a manifest.
	history := []map[string]string{}
	historyReady := false
	ensureHistory := func() error {
		if historyReady {
			return nil
		}
		if err := os.MkdirAll(filepath.Join(outDir, "history"), 0755); err != nil {
			return err
		}
		historyReady = true
		return nil
	}
	for _, e := range snapshot.Evidence {
		if e.ReportPath == "" || core.EvidenceCurrent(snapshot.Goal, status.ActualArtifactID, e) {
			continue
		}
		entry := map[string]string{
			"id": e.ID, "criterion_id": e.CriterionID, "result": e.Result, "invalidated_reason": e.InvalidatedReason,
		}
		if _, err := os.Stat(e.ReportPath); err == nil {
			if err = ensureHistory(); err != nil {
				return status, err
			}
			name := e.ID + "-" + filepath.Base(e.ReportPath)
			if err = copyFile(e.ReportPath, filepath.Join(outDir, "history", name)); err != nil {
				return status, err
			}
			entry["archived_report"] = filepath.Join("history", name)
		}
		history = append(history, entry)
	}
	if historyReady {
		manifest, err := json.MarshalIndent(history, "", "  ")
		if err != nil {
			return status, err
		}
		if err = os.WriteFile(filepath.Join(outDir, "history", "history.json"), append(manifest, '\n'), 0644); err != nil {
			return status, err
		}
	}
	metadata, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return status, err
	}
	if err = os.WriteFile(filepath.Join(outDir, "delivery.json"), append(metadata, '\n'), 0644); err != nil {
		return status, err
	}
	return status, nil
}

func Summary(status Status) string {
	return fmt.Sprintf("goal=%s status=%s verified=%t artifact=%s unverified=%v", status.Snapshot.Goal.ID, status.Snapshot.Goal.Status, status.Verified, status.ActualArtifactID, status.Unverified)
}
