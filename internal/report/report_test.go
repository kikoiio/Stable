package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/core"
)

func TestScreenshotCannotVerifyAndExportTracksCurrentDesign(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design-v1")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	snapshot := core.GoalSnapshot{
		Goal:     core.Goal{ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified},
		Evidence: []core.Evidence{{ID: "screen", Kind: "computer.screenshot", Result: "pass", ArtifactID: digest}},
	}
	first, err := Export(context.Background(), snapshot, filepath.Join(root, "without-erc"))
	if err != nil || first.Verified || len(first.Unverified) == 0 {
		t.Fatalf("screenshot incorrectly verified goal: %+v %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(root, "without-erc", "erc.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected ERC file: %v", err)
	}
	reportPath := filepath.Join(root, "clean-erc.json")
	if err := os.WriteFile(reportPath, []byte(`{"sheets":[{"violations":[]}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot.Evidence = append(snapshot.Evidence, core.Evidence{ID: "erc", Kind: "kicad.erc", Result: "pass", ArtifactID: digest, ReportPath: reportPath})
	verified, err := Export(context.Background(), snapshot, filepath.Join(root, "with-erc"))
	if err != nil || !verified.Verified {
		t.Fatalf("clean current ERC failed verification: %+v %v", verified, err)
	}
	var delivered Status
	encoded, err := os.ReadFile(filepath.Join(root, "with-erc", "delivery.json"))
	if err != nil || json.Unmarshal(encoded, &delivered) != nil || !delivered.Verified || delivered.ActualArtifactID != digest {
		t.Fatalf("invalid delivery metadata: %+v %v", delivered, err)
	}
	if err := os.WriteFile(path, []byte("design-v2"), 0644); err != nil {
		t.Fatal(err)
	}
	stale, err := BuildStatus(snapshot)
	if err != nil || stale.Verified || len(stale.Unverified) < 2 {
		t.Fatalf("stale evidence incorrectly verified goal: %+v %v", stale, err)
	}
}
