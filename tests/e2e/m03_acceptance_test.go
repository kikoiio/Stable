package e2e

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

type m03FindingChecker struct {
	result candidate.FindingResult
}

func (c m03FindingChecker) Check(context.Context, candidate.Candidate) (candidate.Finding, error) {
	return candidate.Finding{ID: "m03-erc", Checker: "m03-test-erc", Version: "fixture-1", Result: c.result, Reason: "fixture result"}, nil
}

func TestM03TrustedCandidateAcceptanceAndRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "board.kicad_sch"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	projectDigest := func() string {
		t.Helper()
		_, digest, err := candidate.BuildManifest(formal)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	before := projectDigest()
	owner, err := sessionlog.Create(root, "m03-acceptance")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state.db")
	state, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	goalID := "m03-acceptance-goal"
	if _, err = state.CreateGoal(ctx, core.Goal{ID: goalID, Objective: "accept candidate", AllowedRoot: formal, SourceSessionID: owner.ID}); err != nil {
		t.Fatal(err)
	}
	candidateRecord, err := candidate.CreateCandidate("m03-acceptance-candidate", formal, filepath.Join(root, ".stable-candidates"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(candidateRecord.CandidateRoot, "board.kicad_sch"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	candidateRecord, err = candidate.FreezeCandidate(candidateRecord, nil, ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidateRecord.Status = "prepared"
	if err = state.SaveCandidate(ctx, store.CandidateRecord{Candidate: candidateRecord, ActionID: "m03-acceptance-action", GoalID: goalID}); err != nil {
		t.Fatal(err)
	}
	if err = state.TransitionCandidate(ctx, candidateRecord.ID, "prepared", "running", ""); err != nil {
		t.Fatal(err)
	}
	if err = state.TransitionCandidate(ctx, candidateRecord.ID, "running", "ready", candidateRecord.CandidateDigest); err != nil {
		t.Fatal(err)
	}

	checker := m03FindingChecker{result: candidate.FindingPass}
	socket := filepath.Join(root, "conversation.sock")
	var serviceCancel context.CancelFunc
	stopService := func() {
		if serviceCancel != nil {
			serviceCancel()
		}
	}
	defer stopService()
	startService := func() {
		t.Helper()
		serviceCtx, cancel := context.WithCancel(ctx)
		serviceCancel = cancel
		_, serveErr := conversation.Serve(serviceCtx, conversation.Deps{Store: state, ProjectRoot: root, RunRoot: filepath.Join(root, "run"), SocketPath: socket, CandidateCheckers: []candidate.Checker{checker}})
		if serveErr != nil {
			t.Fatal(serveErr)
		}
		waitForSocket(t, socket)
	}
	startService()
	connect := func() (net.Conn, *json.Decoder) {
		t.Helper()
		conn, dialErr := net.DialTimeout("unix", socket, 2*time.Second)
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return conn, json.NewDecoder(bufio.NewReader(conn))
	}
	request := func(conn net.Conn, decoder *json.Decoder, msg conversation.ClientMsg, finalType string) conversation.ServerMsg {
		t.Helper()
		if err := json.NewEncoder(conn).Encode(msg); err != nil {
			t.Fatal(err)
		}
		var final conversation.ServerMsg
		for {
			response := receiveServiceMessage(t, conn, decoder)
			if response.Type == "error" {
				t.Fatalf("trusted service %s error: %s", msg.Op, response.Error)
			}
			if response.Type == finalType {
				final = response
			}
			if response.Type == "done" {
				return final
			}
		}
	}

	conn, decoder := connect()
	_ = request(conn, decoder, conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: owner.ID}, "transcript")
	reviewMessage := request(conn, decoder, conversation.ClientMsg{Op: "review_get", SessionID: owner.ID, CandidateID: candidateRecord.ID}, "review")
	_ = conn.Close()
	if reviewMessage.Review == nil || len(reviewMessage.Review.Changes) != 1 || reviewMessage.Review.Changes[0].Status != "modified" {
		t.Fatalf("candidate preview did not show exact modification: %+v", reviewMessage.Review)
	}
	if projectDigest() != before {
		t.Fatal("formal project changed before user acceptance")
	}
	stopService()
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	startService()
	conn, decoder = connect()
	_ = request(conn, decoder, conversation.ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: owner.ID}, "transcript")
	accepted := request(conn, decoder, conversation.ClientMsg{Op: "review_accept", SessionID: owner.ID, CandidateID: candidateRecord.ID, DecisionID: "m03-accept-decision", PreviewDigest: reviewMessage.Review.Digest, CandidateDigest: reviewMessage.Review.CandidateDigest, FormalDigest: reviewMessage.Review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal)}, "acceptance")
	if accepted.Receipt == nil || accepted.Receipt.ID != "receipt-m03-accept-decision" {
		t.Fatalf("trusted acceptance returned no durable receipt: %+v", accepted)
	}
	acceptedAgain := request(conn, decoder, conversation.ClientMsg{Op: "review_accept", SessionID: owner.ID, CandidateID: candidateRecord.ID, DecisionID: "m03-accept-decision", PreviewDigest: reviewMessage.Review.Digest, CandidateDigest: reviewMessage.Review.CandidateDigest, FormalDigest: reviewMessage.Review.FormalDigest, AcceptanceMode: string(candidate.AcceptNormal)}, "acceptance")
	_ = conn.Close()
	if acceptedAgain.Receipt == nil || acceptedAgain.Receipt.ID != accepted.Receipt.ID {
		t.Fatalf("idempotent acceptance changed receipt: %+v", acceptedAgain)
	}
	if projectDigest() == before {
		t.Fatal("accepted candidate did not change the formal project")
	}
	goal, err := state.GetGoalSnapshot(ctx, goalID)
	if err != nil || goal.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("acceptance incorrectly skipped independent reverification: status=%s err=%v", goal.Goal.Status, err)
	}
}

func TestM03ForceAcceptanceRequiresEveryFindingAndKeepsReverification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal, candidateRoot := filepath.Join(root, "project"), filepath.Join(root, "candidate")
	for _, dir := range []string{formal, candidateRoot} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(formal, "board.kicad_sch"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidateRoot, "board.kicad_sch"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	_, formalDigest, _ := candidate.BuildManifest(formal)
	_, candidateDigest, _ := candidate.BuildManifest(candidateRoot)
	record := candidate.Candidate{ID: "m03-force-candidate", FormalRoot: formal, CandidateRoot: candidateRoot, BaselineDigest: formalDigest, CandidateDigest: candidateDigest, Status: "frozen"}
	review, err := candidate.BuildReview(ctx, record, []candidate.Checker{m03FindingChecker{result: candidate.FindingUnavailable}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goalID := "m03-force-goal"
	if _, err = db.CreateGoal(ctx, core.Goal{ID: goalID, AllowedRoot: formal}); err != nil {
		t.Fatal(err)
	}
	record.Status = "reviewed"
	if err = db.SaveCandidate(ctx, store.CandidateRecord{Candidate: record, ActionID: "m03-force-action", GoalID: goalID}); err != nil {
		t.Fatal(err)
	}
	decision := func(id string, confirmed []string) candidate.AcceptanceDecision {
		return candidate.AcceptanceDecision{ID: id, UserID: "local-user", CandidateID: record.ID, CandidateDigest: review.CandidateDigest, PreviewDigest: review.Digest, FormalDigest: review.FormalDigest, Mode: candidate.AcceptForce, ConfirmedFindings: confirmed}
	}
	if _, err = candidate.AcceptCandidate(ctx, record, review, decision("m03-force-incomplete", nil), goalID, "m03-force-action", db, time.Time{}); err == nil {
		t.Fatal("force acceptance without explicit finding confirmation was accepted")
	}
	if _, err = candidate.AcceptCandidate(ctx, record, review, decision("m03-force-complete", []string{"m03-erc"}), goalID, "m03-force-action", db, time.Time{}); err != nil {
		t.Fatal(err)
	}
	goal, err := db.GetGoalSnapshot(ctx, goalID)
	if err != nil || goal.Goal.Status != core.GoalPendingReverification {
		t.Fatalf("force acceptance incorrectly verified goal: status=%s err=%v", goal.Goal.Status, err)
	}
}
