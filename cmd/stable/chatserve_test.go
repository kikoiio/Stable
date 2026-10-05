package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/candidate"
	"stable/internal/conversation"
	"stable/internal/core"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// The chatserve entry assembles the trusted review/acceptance path: an
// independent sandbox checker, startup recovery for interrupted acceptances,
// and a service that only lets the owning session submit decisions.
func TestChatserveAssembly(t *testing.T) {
	checkers := chatCandidateCheckers("/run/root")
	if len(checkers) != 1 {
		t.Fatalf("checkers=%v", checkers)
	}
	erc, ok := checkers[0].(candidate.KicadERCChecker)
	if !ok || erc.Sandbox == nil || erc.RunRoot != "/run/root" {
		t.Fatalf("chatserve checker wiring: %+v", checkers[0])
	}

	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	owner, err := sessionlog.Create(root, "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := sessionlog.Create(root, "other")
	if err != nil {
		t.Fatal(err)
	}
	formal := filepath.Join(root, "formal")
	work := filepath.Join(root, "candidate")
	for _, dir := range []string{formal, work} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(formal, "board"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(work, "board"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "g", AllowedRoot: formal, SourceSessionID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	_, base, _ := candidate.BuildManifest(formal)
	_, digest, _ := candidate.BuildManifest(work)
	c := candidate.Candidate{ID: "c", FormalRoot: formal, CandidateRoot: work, BaselineDigest: base, CandidateDigest: digest, Status: "reviewed"}
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: c, ActionID: "a", GoalID: "g"}); err != nil {
		t.Fatal(err)
	}
	review, err := candidate.BuildReview(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = state.SaveCandidateReview(ctx, review); err != nil {
		t.Fatal(err)
	}

	// Startup recovery: an acceptance interrupted before journaling finalizes
	// here, before the socket serves any new decision.
	interrupted := filepath.Join(root, "interrupted-formal")
	interruptedWork := filepath.Join(root, "interrupted-candidate")
	for _, dir := range []string{interrupted, interruptedWork} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(interrupted, "board"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(interruptedWork, "board"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = state.CreateGoal(ctx, core.Goal{ID: "g2", AllowedRoot: interrupted}); err != nil {
		t.Fatal(err)
	}
	_, oldDigest, _ := candidate.BuildManifest(interrupted)
	_, newDigest, _ := candidate.BuildManifest(interruptedWork)
	ic := candidate.Candidate{ID: "c2", FormalRoot: interrupted, CandidateRoot: interruptedWork, BaselineDigest: oldDigest, CandidateDigest: newDigest, Status: "reviewed"}
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: ic, ActionID: "a2", GoalID: "g2"}); err != nil {
		t.Fatal(err)
	}
	id := candidate.AcceptanceDecision{ID: "d-interrupted", UserID: "user", CandidateID: ic.ID, CandidateDigest: newDigest, PreviewDigest: "preview", FormalDigest: oldDigest, Mode: candidate.AcceptNormal}
	if _, err = state.SaveAcceptanceDecision(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = chatserveRecovery(ctx, state); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(interrupted, "board")); string(data) != "after" {
		t.Fatalf("interrupted acceptance not recovered: %q", data)
	}
	if _, ok, _ := state.FindAcceptanceReceipt(ctx, id.ID); !ok {
		t.Fatal("startup recovery produced no receipt")
	}

	socket := filepath.Join(root, "chat.sock")
	svcCtx, stopSvc := context.WithCancel(ctx)
	defer stopSvc()
	svc, err := conversation.Serve(svcCtx, conversation.Deps{Store: state, ProjectRoot: root, SocketPath: socket, CandidateCheckers: checkers})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	exchange := func(msg conversation.ClientMsg) []conversation.ServerMsg {
		t.Helper()
		conn, err := net.DialTimeout("unix", socket, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = json.NewEncoder(conn).Encode(msg); err != nil {
			t.Fatal(err)
		}
		var out []conversation.ServerMsg
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for scanner.Scan() {
			var m conversation.ServerMsg
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				t.Fatalf("invalid server line: %q", scanner.Text())
			}
			out = append(out, m)
			if m.Type == "done" {
				return out
			}
		}
		t.Fatalf("server closed without done: %+v", out)
		return nil
	}

	accept := func(session, decision string) []conversation.ServerMsg {
		return exchange(conversation.ClientMsg{Op: "review_accept", CandidateID: "c", SessionID: session, DecisionID: decision, PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest, FormalDigest: review.FormalDigest, AcceptanceMode: "normal"})
	}

	// A session that does not own the candidate cannot review or accept it.
	msgs := exchange(conversation.ClientMsg{Op: "review_get", CandidateID: "c", SessionID: other.ID})
	if len(msgs) == 0 || msgs[0].Type != "error" {
		t.Fatalf("foreign session read the review: %+v", msgs)
	}
	msgs = accept(other.ID, "d-1")
	if len(msgs) == 0 || msgs[0].Type != "error" {
		t.Fatalf("foreign session submitted a decision: %+v", msgs)
	}
	if data, _ := os.ReadFile(filepath.Join(formal, "board")); string(data) != "old" {
		t.Fatal("formal project changed by a foreign decision")
	}

	// The owning session submits the same decision ID: the failed foreign
	// attempt must not have reserved it, and the exchange happens exactly once.
	msgs = accept(owner.ID, "d-1")
	if len(msgs) == 0 || msgs[0].Type != "acceptance" || msgs[0].Receipt == nil {
		t.Fatalf("owning session could not accept: %+v", msgs)
	}
	receipt := msgs[0].Receipt.ID
	if data, _ := os.ReadFile(filepath.Join(formal, "board")); string(data) != "new" {
		t.Fatal("formal project was not updated by the trusted decision")
	}
	msgs = accept(owner.ID, "d-1")
	if len(msgs) == 0 || msgs[0].Type != "acceptance" || msgs[0].Receipt == nil || msgs[0].Receipt.ID != receipt {
		t.Fatalf("replayed decision returned a different receipt: %+v", msgs)
	}
	snapshot, err := state.GetGoalSnapshot(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("goal not awaiting independent reverification: %+v", snapshot.Goal)
	}
}

// The chatserve tool whitelist covers the five file tools, the controlled
// command executor and the six M06 interaction/task tools, sorted by name
// without duplicates.
func TestChatserveToolSchemas(t *testing.T) {
	schemas := chatserveToolSchemas()
	want := []string{
		"ask_user", "command", "edit_file", "exit_plan_mode", "glob", "grep",
		"read_file", "task_create", "task_get", "task_list", "task_update",
		"write_file",
	}
	if len(schemas) != len(want) {
		t.Fatalf("schema count = %d, want %d", len(schemas), len(want))
	}
	seen := map[string]bool{}
	for i, schema := range schemas {
		if schema.Name != want[i] {
			t.Fatalf("schemas[%d] = %q, want %q", i, schema.Name, want[i])
		}
		if seen[schema.Name] {
			t.Fatalf("duplicate schema name %q", schema.Name)
		}
		seen[schema.Name] = true
		if schema.Description == "" || schema.InputSchema == nil {
			t.Fatalf("schema %q is missing description or input schema", schema.Name)
		}
	}
}
