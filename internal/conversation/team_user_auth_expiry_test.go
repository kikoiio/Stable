package conversation

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"stable/internal/agent"
)

func TestTeamUserProofEnforcesExpiryAndFutureClockSkew(t *testing.T) {
	service := &Service{}
	request := agent.ExecutionRequest{
		TeamUser: true,
		Work:     agent.WorkRef{Kind: agent.WorkGoal, SessionID: "session", GoalID: "goal", WorkItemID: "item"},
	}
	proofAt := func(issued time.Time) string {
		t.Helper()
		issuedAt := issued.UTC().UnixNano()
		payload, err := json.Marshal(teamUserProofPayload{Version: 1, IssuedAt: issuedAt, Work: request.Work})
		if err != nil {
			t.Fatal(err)
		}
		key, err := service.teamUserSigningKey(true)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(payload)
		return strconv.FormatInt(issuedAt, 10) + "." + hex.EncodeToString(mac.Sum(nil))
	}

	now := time.Now()
	for _, test := range []struct {
		name   string
		issued time.Time
		valid  bool
	}{
		{name: "fresh", issued: now, valid: true},
		{name: "within future skew", issued: now.Add(4 * time.Second), valid: true},
		{name: "expired", issued: now.Add(-teamUserProofLifetime - time.Second), valid: false},
		{name: "beyond future skew", issued: now.Add(6 * time.Second), valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			candidate.TeamUserProof = proofAt(test.issued)
			if got := service.validTeamUserRequest(candidate); got != test.valid {
				t.Fatalf("validTeamUserRequest()=%v, want %v for issuedAt %s", got, test.valid, test.issued)
			}
		})
	}
}
