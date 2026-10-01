package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/core"
)

func currentProvenance(checker string) *core.EvidenceProvenance {
	family := core.CheckFamilyConnection
	if checker == "kicad-cli-erc" {
		family = core.CheckFamilyERC
	}
	dep := dependencyFor(family)
	return &core.EvidenceProvenance{
		SchemaVersion:    2,
		Claim:            "check passes under current criteria",
		Coverage:         "sensor board design",
		CheckerID:        checker,
		CheckerVersion:   "1.0.0",
		SourceLevel:      "tool_check",
		InvalidationRule: "criteria or artifact change",
		Family:           family,
		Dependency:       &dep,
	}
}

func dependencyFor(family core.CheckFamily) core.DependencySnapshot {
	checker, version := "sensor-connection-check", "1.0.0"
	if family == core.CheckFamilyERC {
		checker, version = "kicad-cli-erc", "1.0.0"
	}
	return core.DependencySnapshot{SchemaVersion: 1, Family: family, Sources: []core.DependencySource{{Kind: "fixture", Identity: "fixture", Digest: "digest-" + string(family), State: "available"}}, CheckerID: checker, CheckerVersion: version, Fingerprint: "fingerprint-" + string(family), Available: true}
}

func reportDependencies() []core.DependencySnapshot {
	return []core.DependencySnapshot{dependencyFor(core.CheckFamilyERC), dependencyFor(core.CheckFamilyConnection)}
}

func sensorCriteria(maxViolations int) []core.Criterion {
	payload, _ := json.Marshal(map[string]int{"max_violations": maxViolations})
	return []core.Criterion{
		{ID: "erc-clean", Kind: core.CriterionKindERCClean, Payload: payload},
		{ID: "conn", Kind: core.CriterionKindConnectionPresent, Payload: json.RawMessage(`{"endpoint_a":"RT1.2","endpoint_b":"J1.2"}`)},
	}
}

func currentEvidence(id, criterionID, kind, digest, reportPath string) core.Evidence {
	return core.Evidence{
		ID: id, CriterionID: criterionID, Kind: kind, Result: "pass",
		ArtifactID: digest, ReportPath: reportPath,
		CriteriaRevision: revision(1), Provenance: currentProvenance(checkerFor(kind)),
	}
}

func checkerFor(kind string) string {
	if kind == "kicad.erc" {
		return "kicad-cli-erc"
	}
	return "sensor-connection-check"
}

func revision(rev int) *int { return &rev }

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
		Goal: core.Goal{
			ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified,
			CriteriaRevision: 1, Criteria: sensorCriteria(0),
		},
		Dependencies: reportDependencies(), Evidence: []core.Evidence{{ID: "screen", Kind: "computer.screenshot", Result: "pass", ArtifactID: digest}},
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
	snapshot.Evidence = append(snapshot.Evidence,
		currentEvidence("erc", "erc-clean", "kicad.erc", digest, reportPath),
		currentEvidence("conn", "conn", "sensor.connection_present", digest, ""),
	)
	verified, err := Export(context.Background(), snapshot, filepath.Join(root, "with-erc"))
	if err != nil || !verified.Verified {
		t.Fatalf("clean current evidence failed verification: %+v %v", verified, err)
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

func TestStatusERCRechecksCurrentThreshold(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	reportPath := filepath.Join(root, "erc.json")
	if err := os.WriteFile(reportPath, []byte(`{"sheets":[{"violations":[{"msg":"keepout"}]}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Evidence was produced under an older, looser threshold: original result
	// stays pass, but the current criterion allows zero violations.
	erc := currentEvidence("erc", "erc-clean", "kicad.erc", digest, reportPath)
	snapshot := core.GoalSnapshot{
		Goal: core.Goal{
			ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified,
			CriteriaRevision: 1, Criteria: sensorCriteria(0),
		},
		Dependencies: reportDependencies(), Evidence: []core.Evidence{erc, currentEvidence("conn", "conn", "sensor.connection_present", digest, "")},
	}
	status, err := BuildStatus(snapshot)
	if err != nil || status.Verified {
		t.Fatalf("violation above current threshold verified goal: %+v %v", status, err)
	}
	if !strings.Contains(strings.Join(status.Unverified, "; "), "ERC reports 1 violations, threshold 0") {
		t.Fatalf("threshold mismatch not reported: %v", status.Unverified)
	}
	snapshot.Goal.Criteria = sensorCriteria(1)
	relaxed, err := BuildStatus(snapshot)
	if err != nil || !relaxed.Verified {
		t.Fatalf("same evidence should pass under relaxed current threshold: %+v %v", relaxed, err)
	}
}

func TestExportPendingHasNoCurrentERCAndArchivesHistory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design-v1")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	oldReport := filepath.Join(root, "old-erc.json")
	if err := os.WriteFile(oldReport, []byte(`{"sheets":[{"violations":[]}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	rev1 := 1
	snapshot := core.GoalSnapshot{
		Goal: core.Goal{
			ID: "g", ArtifactPath: path, CurrentArtifactID: digest,
			Status: core.GoalPendingReverification, CriteriaRevision: 2, Criteria: sensorCriteria(0),
			Reason: "标准已升级到 v2，待完整复核",
		},
		Dependencies: reportDependencies(), Evidence: []core.Evidence{
			{
				ID: "old-erc", CriterionID: "erc-clean", Kind: "kicad.erc", Result: "pass",
				ArtifactID: digest, ReportPath: oldReport,
				CriteriaRevision: &rev1, Provenance: currentProvenance("kicad-cli-erc"),
				InvalidatedReason: "标准于确认提案后升级到 v2，原结论需完整复核",
			},
		},
	}
	status, err := Export(context.Background(), snapshot, filepath.Join(root, "delivery"))
	if err != nil || status.Verified {
		t.Fatalf("pending export must not verify: %+v %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "delivery", "erc.json")); !os.IsNotExist(err) {
		t.Fatalf("pending export must not deliver a current ERC report: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "delivery", "history", "history.json"))
	if err != nil {
		t.Fatalf("history manifest missing: %v", err)
	}
	manifestText := string(manifest)
	for _, want := range []string{"\"result\": \"pass\"", "标准于确认提案后升级到 v2", "old-erc-old-erc.json"} {
		if !strings.Contains(manifestText, want) {
			t.Fatalf("history manifest missing %q: %s", want, manifestText)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "delivery", "history", "old-erc-old-erc.json")); err != nil {
		t.Fatalf("legacy report not archived: %v", err)
	}
	if len(status.Evidence) != 1 || status.Evidence[0].Current {
		t.Fatalf("normalized view wrong: %+v", status.Evidence)
	}
	if status.Evidence[0].CriteriaRevision != "1" || status.Evidence[0].SourceLevel != "tool_check" {
		t.Fatalf("known fields not rendered: %+v", status.Evidence[0])
	}
	// A fully legacy row (no revision, no provenance) renders as unknown.
	snapshot.Evidence[0].CriteriaRevision = nil
	snapshot.Evidence[0].Provenance = nil
	view, err := BuildStatus(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if view.Evidence[0].CriteriaRevision != "unknown" || view.Evidence[0].CheckerVersion != "unknown" || view.Evidence[0].SourceLevel != "unknown" {
		t.Fatalf("legacy row not shown as unknown: %+v", view.Evidence[0])
	}
}

func TestStatusRequiresEveryCriterionCurrent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	reportPath := filepath.Join(root, "erc.json")
	if err := os.WriteFile(reportPath, []byte(`{"sheets":[{"violations":[]}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	// Legacy ERC row (no revision, no provenance) reads as unknown and never
	// counts as current.
	legacy := core.Evidence{ID: "legacy-erc", Kind: "kicad.erc", Result: "pass", ArtifactID: digest, ReportPath: reportPath}
	snapshot := core.GoalSnapshot{
		Goal: core.Goal{
			ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified,
			CriteriaRevision: 1, Criteria: sensorCriteria(0),
		},
		Dependencies: reportDependencies(), Evidence: []core.Evidence{legacy},
	}
	status, err := BuildStatus(snapshot)
	if err != nil || status.Verified {
		t.Fatalf("legacy evidence verified goal: %+v %v", status, err)
	}
	joined := strings.Join(status.Unverified, "; ")
	if !strings.Contains(joined, "no current evidence for criterion erc-clean") ||
		!strings.Contains(joined, "no current evidence for criterion conn") {
		t.Fatalf("missing criteria not reported: %v", status.Unverified)
	}
	// Only ERC becomes current while the connection criterion stays without
	// evidence: the goal must remain unverified.
	snapshot.Evidence = append(snapshot.Evidence, currentEvidence("erc", "erc-clean", "kicad.erc", digest, reportPath))
	ercOnly, err := BuildStatus(snapshot)
	if err != nil || ercOnly.Verified {
		t.Fatalf("missing connection evidence verified goal: %+v %v", ercOnly, err)
	}
	joined = strings.Join(ercOnly.Unverified, "; ")
	if !strings.Contains(joined, "no current evidence for criterion conn") ||
		strings.Contains(joined, "no current evidence for criterion erc-clean") {
		t.Fatalf("per-criterion coverage wrong: %v", ercOnly.Unverified)
	}
}

func TestDependencyStatusProjection(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	ercReport := filepath.Join(root, "erc.json")
	if err := os.WriteFile(ercReport, []byte(`{"sheets":[{"violations":[]}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	erc := currentEvidence("erc-v2", "erc-clean", "kicad.erc", digest, ercReport)
	conn := currentEvidence("conn-v2", "conn", "sensor.connection_present", digest, "")
	snapshot := core.GoalSnapshot{Goal: core.Goal{ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified, CriteriaRevision: 1, DependencyRevision: 2, Criteria: sensorCriteria(0)}, Dependencies: reportDependencies(), Evidence: []core.Evidence{erc, conn}}
	snapshot.Dependencies[0].Fingerprint = "new-project-setting"
	snapshot.Dependencies[0].Available = false
	snapshot.Dependencies[0].Reason = "selected global symbol table is missing"
	status, err := BuildStatus(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if status.Verified {
		t.Fatalf("changed ERC dependency retained verified status: %+v", status)
	}
	if status.Evidence[0].Current || !status.Evidence[1].Current {
		t.Fatalf("dependency did not invalidate only ERC: %+v", status.Evidence)
	}
	if status.Evidence[0].DependencyFingerprint != "fingerprint-kicad.erc" || status.Evidence[0].DesignDigest != digest || status.Evidence[0].CriteriaRevision != "1" || status.Evidence[0].Dependency == nil || status.Evidence[0].Dependency.CheckerVersion != "1.0.0" {
		t.Fatalf("v2 provenance missing from projection: %+v", status.Evidence[0])
	}
	if !strings.Contains(strings.Join(status.Unverified, "; "), "no current evidence for criterion erc-clean") {
		t.Fatalf("missing ERC evidence gap: %v", status.Unverified)
	}
	if !strings.Contains(strings.Join(status.Unverified, "; "), "selected global symbol table is missing") {
		t.Fatalf("unavailable dependency reason omitted: %v", status.Unverified)
	}
	legacy := core.Evidence{ID: "v1", CriterionID: "erc-clean", Kind: "kicad.erc", Result: "pass", ArtifactID: digest, CriteriaRevision: revision(1), Provenance: currentProvenance("kicad-cli-erc")}
	legacy.Provenance.SchemaVersion = 1
	legacy.Provenance.Dependency = nil
	projected := normalizeEvidence(snapshot, digest, legacy)
	if projected.Current || projected.DependencyFingerprint != "unknown" || projected.CheckerVersion != "1.0.0" {
		t.Fatalf("legacy provenance should remain known except dependency: %+v", projected)
	}
}

func TestStatusHandlesUnknownCheckerAndMissingReport(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sensor.kicad_sch")
	data := []byte("design")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	// Checker version unknown (empty field) disqualifies otherwise passing
	// evidence; the report file for the ERC criterion is also missing.
	unknownChecker := currentEvidence("erc", "erc-clean", "kicad.erc", digest, filepath.Join(root, "gone.json"))
	unknownChecker.Provenance.CheckerVersion = ""
	snapshot := core.GoalSnapshot{
		Goal: core.Goal{
			ID: "g", ArtifactPath: path, CurrentArtifactID: digest, Status: core.GoalVerified,
			CriteriaRevision: 1, Criteria: sensorCriteria(0),
		},
		Dependencies: reportDependencies(), Evidence: []core.Evidence{
			unknownChecker,
			currentEvidence("conn", "conn", "sensor.connection_present", digest, ""),
		},
	}
	status, err := BuildStatus(snapshot)
	if err != nil || status.Verified {
		t.Fatalf("unknown checker version verified goal: %+v %v", status, err)
	}
	if !strings.Contains(strings.Join(status.Unverified, "; "), "no current evidence for criterion erc-clean") {
		t.Fatalf("unknown checker not rejected: %v", status.Unverified)
	}
	// Restore the checker version; the evidence becomes current but the report
	// file is gone, so the ERC criterion cannot be re-checked.
	fresh := currentEvidence("erc", "erc-clean", "kicad.erc", digest, filepath.Join(root, "gone.json"))
	snapshot.Evidence[0] = fresh
	status, err = BuildStatus(snapshot)
	if err != nil || status.Verified {
		t.Fatalf("missing report verified goal: %+v %v", status, err)
	}
	if !strings.Contains(strings.Join(status.Unverified, "; "), "ERC report unreadable") {
		t.Fatalf("missing report not reported: %v", status.Unverified)
	}
}
