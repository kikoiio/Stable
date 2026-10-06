package execution

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"stable/internal/permission"
	"stable/internal/platform/sandbox"
)

// PinNetworkGrants canonicalizes and pins every grant for one isolated
// invocation. A fresh lookup per invocation keeps the trusted profile tied to
// the approval's current DNS answer; the proxy performs a second lookup before
// every dial.
func PinNetworkGrants(ctx context.Context, grants []permission.NetworkGrant, resolver sandbox.GrantResolver) ([]permission.NetworkGrant, error) {
	if len(grants) == 0 {
		return nil, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	result := make([]permission.NetworkGrant, 0, len(grants))
	for _, grant := range grants {
		pinned, err := sandbox.PinNetworkGrant(ctx, grant, resolver)
		if err != nil {
			return nil, fmt.Errorf("pin network authorization for %s:%d: %w", grant.Host, grant.Port, err)
		}
		result = append(result, pinned)
	}
	return result, nil
}

// OneShotSandboxProfile is the single profile constructor for commands and
// non-persistent bridges. It owns the trusted roots and pins any approved
// network grants before the sandbox manager sees them.
func OneShotSandboxProfile(ctx context.Context, authority permission.Authority, runRoot, helperPath string, timeout time.Duration, outputLimit int, resolver sandbox.GrantResolver) (sandbox.SandboxProfile, error) {
	if authority.AllowedRoot == "" || authority.CandidateRoot == "" || runRoot == "" {
		return sandbox.SandboxProfile{}, fmt.Errorf("sandbox authority lacks formal, candidate, or run root")
	}
	formal, err := filepath.Abs(authority.AllowedRoot)
	if err != nil {
		return sandbox.SandboxProfile{}, err
	}
	candidate, err := filepath.Abs(authority.CandidateRoot)
	if err != nil {
		return sandbox.SandboxProfile{}, err
	}
	run, err := filepath.Abs(runRoot)
	if err != nil {
		return sandbox.SandboxProfile{}, err
	}
	grants, err := PinNetworkGrants(ctx, authority.Network, resolver)
	if err != nil {
		return sandbox.SandboxProfile{}, err
	}
	profile := sandbox.SandboxProfile{
		ProjectRoot:     formal,
		CandidateRoot:   candidate,
		RunRoot:         run,
		Timeout:         timeout,
		OutputLimit:     outputLimit,
		NetworkGrants:   grants,
		ProxyHelperPath: helperPath,
	}
	return profile, nil
}

// RejectPersistentNetwork keeps the persistent-session contract explicit at
// the execution boundary instead of relying only on the Linux implementation
// to reject a late profile.
func RejectPersistentNetwork(authority permission.Authority) error {
	if len(authority.Network) != 0 {
		return fmt.Errorf("%w: %d grant(s) supplied", sandbox.ErrSessionNetworkUnsupported, len(authority.Network))
	}
	return nil
}

var _ sandbox.GrantResolver = (*net.Resolver)(nil)
