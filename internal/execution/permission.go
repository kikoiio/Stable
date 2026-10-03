package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"stable/internal/permission"
)

type permissionRepository interface {
	permission.ApprovalRepository
	ListExactRules(context.Context, string) ([]permission.ExactRule, error)
}

// StorePermissionGate applies the same persisted exact-rule policy as the
// conversation approval endpoint before the legacy bridge gets candidate access.
type StorePermissionGate struct {
	Store permissionRepository
	NewID func() string
}

func (g StorePermissionGate) Authorize(ctx context.Context, authority permission.Authority, operation permission.Operation) (permission.PermissionDecision, error) {
	if g.Store == nil {
		return permission.PermissionDecision{}, errors.New("permission store is unavailable")
	}
	scope, err := authority.ScopeDigest()
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	rules, err := g.Store.ListExactRules(ctx, scope)
	if err != nil {
		return permission.PermissionDecision{}, err
	}
	newID := g.NewID
	if newID == nil {
		newID = func() string {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return ""
			}
			return "approval-" + hex.EncodeToString(b[:])
		}
	}
	service := permission.PermissionService{Policy: permission.Policy{Rules: rules}, Repository: g.Store, NewID: newID}
	return service.Authorize(ctx, authority, operation)
}
