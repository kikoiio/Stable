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

	"stable/internal/platform/kicad"
	"stable/internal/platform/sandbox"
	"stable/internal/platform/secfile"
)

var discoverKicadCapabilities = kicad.Discover

// KicadERCChecker runs the installed KiCad ERC against candidate schematics
// inside the verified Linux sandbox. Reports and KiCad user state stay under
// the private run root.
type KicadERCChecker struct {
	Sandbox      sandbox.SandboxManager
	RunRoot      string
	Timeout      time.Duration
	OutputLimit  int
	Capabilities *kicad.Capabilities
}

// seedKicadConfig prepares a minimal KiCad user configuration inside the
// private run root, copying the distribution template library tables when
// available (the same source workers/kicad uses).
func seedKicadConfig(runRoot, templateRoot string) error {
	for _, dir := range []string{"config", "cache", "data", "templates"} {
		if err := secfile.MkdirAllPrivate(filepath.Join(runRoot, dir), 0700); err != nil {
			return err
		}
	}
	config := filepath.Join(runRoot, "config", "kicad", "9.0")
	if err := secfile.MkdirAllPrivate(config, 0700); err != nil {
		return err
	}
	for _, name := range []string{"sym-lib-table", "fp-lib-table"} {
		dest := filepath.Join(config, name)
		if info, err := os.Lstat(dest); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return secfile.ErrUnsafePath
			}
			continue
		}
		template, err := secfile.OpenNoFollow(filepath.Join(templateRoot, name))
		if err != nil {
			continue
		}
		info, err := template.Stat()
		if err != nil {
			_ = template.Close()
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			_ = template.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(template, (1<<20)+1))
		closeErr := template.Close()
		if readErr != nil || closeErr != nil || len(data) > 1<<20 {
			continue
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (k KicadERCChecker) resolveCapabilities(ctx context.Context, profile sandbox.SandboxProfile) (kicad.Capabilities, error) {
	if k.Capabilities != nil {
		return *k.Capabilities, nil
	}
	return discoverKicadCapabilities(ctx, k.Sandbox, profile)
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
	baseRoot, err := secfile.OpenRoot(baseRunRoot)
	if err != nil {
		return Finding{}, fmt.Errorf("checker run root is unsafe: %w", err)
	}
	runRoot, err = os.MkdirTemp(baseRunRoot, "check-")
	if err != nil {
		return Finding{}, err
	}
	if err = secfile.ChmodPrivate(runRoot, 0700); err != nil {
		return Finding{}, err
	}
	if err := baseRoot.Revalidate(); err != nil {
		return Finding{}, err
	}
	runRootSecure, err := secfile.OpenRoot(runRoot)
	if err != nil {
		return Finding{}, fmt.Errorf("checker run directory is unsafe: %w", err)
	}
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	outputLimit := k.OutputLimit
	if outputLimit <= 0 {
		outputLimit = 1 << 20
	}
	resolverProfile := sandbox.SandboxProfile{ProjectRoot: c.FormalRoot, CandidateRoot: c.CandidateRoot, RunRoot: runRoot, Timeout: timeout, OutputLimit: outputLimit}
	capabilities, discoverErr := k.resolveCapabilities(ctx, resolverProfile)
	if discoverErr != nil {
		return Finding{}, fmt.Errorf("discover KiCad ERC capabilities: %w", discoverErr)
	}
	if !capabilities.AvailableFor(string(kicad.KindERC)) {
		return Finding{}, fmt.Errorf("KiCad ERC is unavailable: missing %s", strings.Join(capabilities.Missing(string(kicad.KindERC)), ", "))
	}
	version := strings.TrimSpace(capabilities.CLI.Version)
	if version == "" || len(version) > 128 {
		return Finding{}, errors.New("kicad-cli returned an invalid version")
	}
	// kicad-cli with the sandbox's empty HOME creates an empty symbol library
	// table and then reports every symbol as a lib_symbol_issues violation.
	// Seed the resolver-selected template tables into the private run root.
	if err = seedKicadConfig(runRoot, capabilities.TemplateRoot); err != nil {
		return Finding{}, fmt.Errorf("seed KiCad checker config: %w", err)
	}
	profile := sandbox.SandboxProfile{ProjectRoot: c.FormalRoot, CandidateRoot: c.CandidateRoot, RunRoot: runRoot, Timeout: timeout, OutputLimit: outputLimit,
		Environment: capabilities.Environment("/workspace/run")}
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
		reportFile, readErr := runRootSecure.Open(filepath.Base(report))
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
		if err := runRootSecure.Revalidate(); err != nil {
			return Finding{}, err
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
	if err := runRootSecure.Revalidate(); err != nil {
		return Finding{}, err
	}
	result := FindingPass
	reason := "KiCad ERC reported no violations"
	if violations > 0 {
		result = FindingFail
		reason = fmt.Sprintf("KiCad ERC reported %d violation(s)", violations)
	}
	return Finding{ID: "kicad-erc-" + c.ID, Checker: "kicad-cli-erc", Version: version, Result: result, Files: files, Reason: reason}, nil
}
