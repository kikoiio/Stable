package execution

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
	"stable/internal/platform/sandbox"
)

type profileResolver map[string][]netip.Addr

func (r profileResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	ips, ok := r[host]
	if !ok {
		return nil, errors.New("host not found")
	}
	return append([]netip.Addr(nil), ips...), nil
}

func TestOneShotSandboxProfilePinsAuthorityGrants(t *testing.T) {
	root := t.TempDir()
	authority := permission.Authority{
		AllowedRoot:   filepath.Join(root, "formal"),
		CandidateRoot: filepath.Join(root, "candidate"),
		Network:       []permission.NetworkGrant{{Protocol: "TCP", Host: "Target.TEST.", Port: 443}},
	}
	resolver := profileResolver{"target.test": {netip.MustParseAddr("127.0.0.1")}}
	profile, err := OneShotSandboxProfile(context.Background(), authority, filepath.Join(root, "run"), "/bin/true", 3*time.Second, 4096, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.NetworkGrants) != 1 || profile.NetworkGrants[0].Host != "target.test" || profile.NetworkGrants[0].Protocol != "tcp" || len(profile.NetworkGrants[0].ResolvedIPs) != 1 {
		t.Fatalf("profile did not contain canonical pinned grant: %+v", profile.NetworkGrants)
	}
	if profile.Timeout != 3*time.Second || profile.OutputLimit != 4096 || profile.ProxyHelperPath != "/bin/true" {
		t.Fatalf("profile options were not preserved: %+v", profile)
	}
}

func TestOneShotSandboxProfileRejectsUnresolvableGrant(t *testing.T) {
	root := t.TempDir()
	authority := permission.Authority{
		AllowedRoot:   filepath.Join(root, "formal"),
		CandidateRoot: filepath.Join(root, "candidate"),
		Network:       []permission.NetworkGrant{{Protocol: "tcp", Host: "missing.test", Port: 443}},
	}
	_, err := OneShotSandboxProfile(context.Background(), authority, filepath.Join(root, "run"), "/bin/true", 0, 0, profileResolver{})
	if !errors.Is(err, sandbox.ErrNetworkGrantInvalid) {
		t.Fatalf("grant failure was not classified: %v", err)
	}
}

func TestRejectPersistentNetwork(t *testing.T) {
	if err := RejectPersistentNetwork(permission.Authority{}); err != nil {
		t.Fatal(err)
	}
	err := RejectPersistentNetwork(permission.Authority{Network: []permission.NetworkGrant{{Protocol: "tcp", Host: "target.test", Port: 443}}})
	if !errors.Is(err, sandbox.ErrSessionNetworkUnsupported) {
		t.Fatalf("persistent grant was not rejected: %v", err)
	}
}

func TestOneShotSandboxProfileUsesAbsoluteRoots(t *testing.T) {
	root := t.TempDir()
	authority := permission.Authority{AllowedRoot: ".", CandidateRoot: filepath.Join(root, "candidate")}
	profile, err := OneShotSandboxProfile(context.Background(), authority, filepath.Join(root, "run"), "", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(profile.ProjectRoot) || !filepath.IsAbs(profile.CandidateRoot) || !filepath.IsAbs(profile.RunRoot) {
		t.Fatalf("profile roots are not absolute: %+v", profile)
	}
	if _, err := os.Stat(profile.ProjectRoot); err != nil {
		t.Fatalf("absolute project root should resolve: %v", err)
	}
}
