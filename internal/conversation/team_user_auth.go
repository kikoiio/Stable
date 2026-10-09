package conversation

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"stable/internal/agent"
)

const teamUserProofLifetime = 30 * time.Second

var errInvalidTeamUserProof = errors.New("invalid local team user proof")

type teamUserProofPayload struct {
	Version  int           `json:"version"`
	IssuedAt int64         `json:"issued_at"`
	Work     agent.WorkRef `json:"work"`
}

// signTeamUserRequest marks a request reconstructed by the local socket path.
// The proof is deliberately excluded from JSON and can only be produced with
// the per-Service secret.
func (s *Service) signTeamUserRequest(request *agent.ExecutionRequest) error {
	if request == nil || !request.TeamUser {
		return errInvalidTeamUserProof
	}
	issuedAt := time.Now().UTC().UnixNano()
	payload, err := json.Marshal(teamUserProofPayload{Version: 1, IssuedAt: issuedAt, Work: request.Work})
	if err != nil {
		return err
	}
	key, err := s.teamUserSigningKey(true)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	request.TeamUserProof = strconv.FormatInt(issuedAt, 10) + "." + hex.EncodeToString(mac.Sum(nil))
	return nil
}

func (s *Service) validTeamUserRequest(request agent.ExecutionRequest) bool {
	if !request.TeamUser || request.TeamUserProof == "" {
		return false
	}
	parts := strings.Split(request.TeamUserProof, ".")
	if len(parts) != 2 {
		return false
	}
	issuedAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	providedMAC, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	issued := time.Unix(0, issuedAt)
	now := time.Now()
	if issued.After(now.Add(5*time.Second)) || now.Sub(issued) > teamUserProofLifetime {
		return false
	}
	payload, err := json.Marshal(teamUserProofPayload{Version: 1, IssuedAt: issuedAt, Work: request.Work})
	if err != nil {
		return false
	}
	key, err := s.teamUserSigningKey(false)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return hmac.Equal(providedMAC, mac.Sum(nil))
}

func (s *Service) teamUserSigningKey(create bool) ([]byte, error) {
	s.teamUserMu.Lock()
	defer s.teamUserMu.Unlock()
	if !s.teamUserReady {
		if !create {
			return nil, errInvalidTeamUserProof
		}
		if _, err := rand.Read(s.teamUserSecret[:]); err != nil {
			return nil, err
		}
		s.teamUserReady = true
	}
	return append([]byte(nil), s.teamUserSecret[:]...), nil
}
