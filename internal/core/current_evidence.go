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
	if evidence.Result != "pass" || evidence.InvalidatedReason != "" {
		return false
	}
	if evidence.CriteriaRevision == nil || *evidence.CriteriaRevision != goal.CriteriaRevision {
		return false
	}
	if artifactID == "" || evidence.ArtifactID != artifactID {
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
