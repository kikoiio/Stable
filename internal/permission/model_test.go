package permission

import "testing"

func TestDigestStable(t *testing.T) {
	a := Authority{RunID: "r1", SessionID: "s1", AllowedRoot: "/project", CandidateRoot: "/candidate", Network: []NetworkGrant{{Protocol: "tcp", Host: "example.test", Port: 443}}}
	b := a
	d1, err := a.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := b.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("same scope produced different digests: %s != %s", d1, d2)
	}
	b.AllowedRoot = "/other-project"
	d3, err := b.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d3 {
		t.Fatal("scope change did not change digest")
	}
}

func TestOperationDigestChangesWithParameters(t *testing.T) {
	a := Operation{ID: "op1", Kind: OpCommand, Name: "exec", Parameters: []byte(`{"argv":["true"]}`)}
	d1, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	a.Parameters = []byte(`{"argv":["false"]}`)
	d2, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("parameter change did not change operation digest")
	}
}

// The plan file path is a transient per-session path like the candidate root;
// it must not change the saved-rule scope digest.
func TestScopeDigestIgnoresPlanFilePath(t *testing.T) {
	a := Authority{RunID: "r1", SessionID: "s1", AllowedRoot: "/project", CandidateRoot: "/candidate", Mode: ModePlan, PlanFilePath: "/project/.stable/plans/s1.md"}
	b := a
	d1, err := a.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	b.PlanFilePath = ""
	d2, err := b.ScopeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatal("plan file path changed the saved-rule scope digest")
	}
}
