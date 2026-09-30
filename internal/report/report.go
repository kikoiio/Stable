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
	Snapshot         core.GoalSnapshot `json:"snapshot"`
	ActualArtifactID string            `json:"actual_artifact_id"`
	Verified         bool              `json:"verified"`
	Unverified       []string          `json:"unverified"`
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
	validERC := false
	for _, e := range snapshot.Evidence {
		if e.Kind == "kicad.erc" && e.Result == "pass" && e.ArtifactID == out.ActualArtifactID {
			if reportIsClean(e.ReportPath) {
				validERC = true
				break
			}
		}
	}
	if !validERC {
		out.Unverified = append(out.Unverified, "no clean KiCad ERC for current design")
	}
	if snapshot.Goal.Status != core.GoalVerified {
		out.Unverified = append(out.Unverified, "goal status is "+string(snapshot.Goal.Status))
	}
	out.Verified = len(out.Unverified) == 0
	return out, nil
}

func reportIsClean(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var report struct {
		Sheets []struct {
			Violations []json.RawMessage `json:"violations"`
		} `json:"sheets"`
	}
	if json.Unmarshal(data, &report) != nil || report.Sheets == nil {
		return false
	}
	for _, sheet := range report.Sheets {
		if len(sheet.Violations) > 0 {
			return false
		}
	}
	return true
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
	for _, e := range snapshot.Evidence {
		if e.Kind == "kicad.erc" && e.ArtifactID == status.ActualArtifactID && e.Result == "pass" && reportIsClean(e.ReportPath) {
			if err = copyFile(e.ReportPath, filepath.Join(outDir, "erc.json")); err != nil {
				return status, err
			}
			break
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
