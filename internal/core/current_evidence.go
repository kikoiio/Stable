package core

// EvidenceCurrentWithDependencies checks v2 evidence against the current
// per-family dependency snapshots. Legacy v1 evidence remains readable, but
// can never support a current conclusion because it has no dependency version.
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
