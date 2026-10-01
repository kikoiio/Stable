package core

// EvidenceCurrent reports whether a piece of evidence supports the goal's
// current conclusion. It is the generic filter shared by the model context and
// status/report surfaces; it contains no checker-specific logic.
//
// Evidence is current only when all of the following hold:
//  1. the original check passed and the evidence has not been invalidated;
//  2. the criteria revision is known and equals the goal's current revision;
//  3. the evidence is bound to the current artifact digest;
//  4. the provenance is complete (all required fields non-empty, schema
//     version 1) and the source level is tool_check.
//
// Screenshots and model statements (any source level other than tool_check)
// never qualify, and rows predating revision tracking (nil revision or
// provenance) read as unknown and never qualify.
func EvidenceCurrent(goal Goal, artifactID string, evidence Evidence) bool {
	if !evidenceBaseCurrent(goal, artifactID, evidence) {
		return false
	}
	p := evidence.Provenance
	if p == nil || p.SchemaVersion != 1 {
		return false
	}
	if p.Claim == "" || p.Coverage == "" || p.CheckerID == "" || p.CheckerVersion == "" || p.InvalidationRule == "" {
		return false
	}
	return p.SourceLevel == "tool_check"
}

// EvidenceCurrentWithDependencies checks v2 evidence against the current
// per-family dependency snapshots. The v1 EvidenceCurrent entry point remains
// available while callers migrate to snapshot-aware projection.
func EvidenceCurrentWithDependencies(goal Goal, artifactID string, dependencies []DependencySnapshot, evidence Evidence) bool {
	if !evidenceBaseCurrent(goal, artifactID, evidence) {
		return false
	}
	p := evidence.Provenance
	if p == nil || p.SchemaVersion != 2 || p.SourceLevel != "tool_check" || p.Claim == "" || p.Coverage == "" || p.CheckerID == "" || p.CheckerVersion == "" || p.InvalidationRule == "" {
		return false
	}
	if p.Family != CheckFamilyERC && p.Family != CheckFamilyConnection {
		return false
	}
	evidenceSnapshot := p.Dependency
	if evidenceSnapshot == nil || evidenceSnapshot.SchemaVersion != 1 || evidenceSnapshot.Family != p.Family || !evidenceSnapshot.Available || evidenceSnapshot.Fingerprint == "" {
		return false
	}
	var current *DependencySnapshot
	for i := range dependencies {
		if dependencies[i].Family != p.Family {
			continue
		}
		if current != nil {
			return false
		}
		current = &dependencies[i]
	}
	if current == nil || current.SchemaVersion != evidenceSnapshot.SchemaVersion || !current.Available || current.Fingerprint == "" {
		return false
	}
	return current.Fingerprint == evidenceSnapshot.Fingerprint &&
		current.CheckerID == evidenceSnapshot.CheckerID && current.CheckerVersion == evidenceSnapshot.CheckerVersion &&
		p.CheckerID == evidenceSnapshot.CheckerID && p.CheckerVersion == evidenceSnapshot.CheckerVersion
}

func evidenceBaseCurrent(goal Goal, artifactID string, evidence Evidence) bool {
	if evidence.Result != "pass" || evidence.InvalidatedReason != "" {
		return false
	}
	if evidence.CriteriaRevision == nil || *evidence.CriteriaRevision != goal.CriteriaRevision {
		return false
	}
	return artifactID != "" && evidence.ArtifactID == artifactID
}
