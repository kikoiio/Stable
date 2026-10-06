package candidate

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
	"strings"
	"time"

	"stable/internal/platform/sandbox"
	"stable/internal/platform/secfile"
)

// KicadERCChecker runs the installed KiCad ERC against candidate schematics
// inside the verified Linux sandbox. Reports and KiCad user state stay under
// the private run root.
type KicadERCChecker struct {
	Sandbox     sandbox.SandboxManager
	RunRoot     string
	Timeout     time.Duration
	OutputLimit int
}

// seedKicadConfig prepares a minimal KiCad user configuration inside the
// private run root, copying the distribution template library tables when
// available (the same source workers/kicad uses).
func seedKicadConfig(runRoot string) error {
	config := filepath.Join(runRoot, ".kicad-config", "kicad", "9.0")
	if err := secfile.MkdirAllPrivate(config, 0700); err != nil {
		return err
	}
	for _, name := range []string{"sym-lib-table", "fp-lib-table"} {
		dest := filepath.Join(config, name)
		if _, err := os.Lstat(dest); err == nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/usr/share/kicad/template", name))
		if err != nil {
			continue
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (k KicadERCChecker) Check(ctx context.Context, c Candidate) (Finding, error) {
	if k.Sandbox == nil {
		return Finding{}, errors.New("isolated KiCad checker is unavailable")
	}
	if c.ID == "" || c.FormalRoot == "" || c.CandidateRoot == "" || c.Status != "frozen" && c.Status != "reviewed" {
		return Finding{}, errors.New("KiCad checker requires a frozen candidate")
	}
	formal, formalBefore, err := BuildManifest(c.FormalRoot)
	if err != nil {
		return Finding{}, err
	}
	proposed, candidateBefore, err := BuildManifest(c.CandidateRoot)
	if err != nil {
		return Finding{}, err
	}
	changes, err := DiffManifests(c.FormalRoot, c.CandidateRoot, formal, proposed)
	if err != nil {
		return Finding{}, err
	}
	files := make([]string, 0)
	for _, change := range changes {
		if strings.EqualFold(filepath.Ext(change.Path), ".kicad_sch") {
			files = append(files, change.Path)
		}
	}
	if len(files) == 0 {
		return Finding{ID: "kicad-erc-" + c.ID, Checker: "kicad-cli-erc", Version: "not-run", Result: FindingUnavailable, Reason: "candidate has no changed KiCad schematic to check"}, nil
	}
	runRoot := k.RunRoot
	if runRoot == "" {
		runRoot = filepath.Join(filepath.Dir(c.CandidateRoot), ".stable-review")
	}
	if filepath.Base(c.ID) != c.ID || c.ID == "." || c.ID == ".." || strings.ContainsAny(c.ID, `/\`) {
		return Finding{}, errors.New("candidate ID is unsafe for checker output")
	}
	baseRunRoot := filepath.Join(runRoot, c.ID)
	if err = secfile.MkdirAllPrivate(baseRunRoot, 0700); err != nil {
		return Finding{}, err
	}
	if err = secfile.ChmodPrivate(baseRunRoot, 0700); err != nil {
		return Finding{}, err
	}
	runRoot, err = os.MkdirTemp(baseRunRoot, "check-")
	if err != nil {
		return Finding{}, err
	}
	if err = secfile.ChmodPrivate(runRoot, 0700); err != nil {
		return Finding{}, err
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	outputLimit := k.OutputLimit
	if outputLimit <= 0 {
		outputLimit = 1 << 20
	}
	// kicad-cli with the sandbox's empty HOME creates an empty symbol library
	// table and then reports every symbol as a lib_symbol_issues violation.
	// Seed the standard KiCad template tables into the private run root and
	// point the XDG dirs at them, mirroring workers/kicad kicad_environment.
	if err = seedKicadConfig(runRoot); err != nil {
		return Finding{}, fmt.Errorf("seed KiCad checker config: %w", err)
	}
	profile := sandbox.SandboxProfile{ProjectRoot: c.FormalRoot, CandidateRoot: c.CandidateRoot, RunRoot: runRoot, Timeout: timeout, OutputLimit: outputLimit,
		Environment: []string{
			"XDG_CONFIG_HOME=/workspace/run/.kicad-config",
			"XDG_CACHE_HOME=/workspace/run/.kicad-cache",
			"XDG_DATA_HOME=/workspace/run/.kicad-data",
		}}
	versionResult, err := k.Sandbox.RunIsolated(ctx, profile, []string{"kicad-cli", "version"}, nil)
	if err != nil {
		return Finding{}, fmt.Errorf("probe KiCad checker version: %w", err)
	}
	if versionResult.ExitCode != 0 {
		return Finding{}, errors.New("kicad-cli version command failed")
	}
	version := strings.TrimSpace(string(versionResult.Stdout))
	if version == "" || len(version) > 128 {
		return Finding{}, errors.New("kicad-cli returned an invalid version")
	}
	violations := 0
	for _, rel := range files {
		sum := sha256.Sum256([]byte(rel))
		report := filepath.Join(runRoot, "erc-"+hex.EncodeToString(sum[:8])+".json")
		guestPath := filepath.Join("/workspace/candidate", rel)
		guestReport := filepath.Join("/workspace/run", filepath.Base(report))
		result, runErr := k.Sandbox.RunIsolated(ctx, profile, []string{"kicad-cli", "sch", "erc", "--format", "json", "--severity-all", "--exit-code-violations", "--output", guestReport, guestPath}, nil)
		if runErr != nil {
			return Finding{}, fmt.Errorf("run KiCad ERC for %s: %w", rel, runErr)
		}
		if result.ExitCode != 0 && result.ExitCode != 5 {
			return Finding{}, fmt.Errorf("KiCad ERC failed for %s with exit code %d", rel, result.ExitCode)
		}
		reportFile, readErr := secfile.OpenNoFollow(report)
		if readErr != nil {
			return Finding{}, fmt.Errorf("read KiCad ERC report for %s: %w", rel, readErr)
		}
		reportInfo, readErr := reportFile.Stat()
		if readErr != nil {
			_ = reportFile.Close()
			return Finding{}, readErr
		}
		if !reportInfo.Mode().IsRegular() || reportInfo.Size() > 8<<20 {
			_ = reportFile.Close()
			return Finding{}, fmt.Errorf("KiCad ERC report for %s is unsafe or too large", rel)
		}
		reportData, readErr := io.ReadAll(io.LimitReader(reportFile, (8<<20)+1))
		closeErr := reportFile.Close()
		if readErr != nil || closeErr != nil || len(reportData) > 8<<20 {
			return Finding{}, fmt.Errorf("read KiCad ERC report for %s failed or exceeded its limit", rel)
		}
		var parsed struct {
			Sheets []struct {
				Violations []json.RawMessage `json:"violations"`
			} `json:"sheets"`
		}
		if err = json.Unmarshal(reportData, &parsed); err != nil || len(parsed.Sheets) == 0 {
			return Finding{}, fmt.Errorf("invalid KiCad ERC report for %s", rel)
		}
		for _, sheet := range parsed.Sheets {
			violations += len(sheet.Violations)
		}
	}
	_, formalAfter, err := BuildManifest(c.FormalRoot)
	if err != nil {
		return Finding{}, err
	}
	_, candidateAfter, err := BuildManifest(c.CandidateRoot)
	if err != nil {
		return Finding{}, err
	}
	if formalAfter != formalBefore {
		return Finding{}, errors.New("formal project changed while KiCad ERC was running")
	}
	if candidateAfter != candidateBefore {
		return Finding{}, errors.New("candidate changed while KiCad ERC was running")
	}
	result := FindingPass
	reason := "KiCad ERC reported no violations"
	if violations > 0 {
		result = FindingFail
		reason = fmt.Sprintf("KiCad ERC reported %d violation(s)", violations)
	}
	return Finding{ID: "kicad-erc-" + c.ID, Checker: "kicad-cli-erc", Version: version, Result: result, Files: files, Reason: reason}, nil
}
