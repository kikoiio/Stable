package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"stable/internal/platform/secfile"
)

func NewID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// Layout receives only runtime-owned roots. StateRoot is the dedicated
// workspace parent (normally StateDir()/workspaces), not the formal project.
type Layout struct {
	stateRoot, formalRoot, projectID string
	stateIdentity                    secfile.Root
	projectIdentity                  secfile.Root
}

func NewLayout(stateRoot, formalRoot, projectID string) (*Layout, error) {
	if !ValidID(projectID) || stateRoot == "" || formalRoot == "" {
		return nil, ErrOwnership
	}
	state, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, err
	}
	formal, err := filepath.Abs(formalRoot)
	if err != nil {
		return nil, err
	}
	if contains(state, formal) || contains(formal, state) {
		return nil, ErrUnsafePath
	}
	if err := validateAncestors(formal, false); err != nil {
		return nil, err
	}
	formalHandle, err := secfile.OpenRoot(formal)
	if err != nil {
		return nil, err
	}
	defer formalHandle.Close()
	if err := makePrivateDirectory(state); err != nil {
		return nil, err
	}
	identity, err := secfile.OpenRoot(state)
	if err != nil {
		return nil, err
	}
	project := filepath.Join(state, projectID)
	if err := makePrivateDirectory(project); err != nil {
		return nil, err
	}
	if err := identity.Revalidate(); err != nil {
		return nil, err
	}
	projectIdentity, err := secfile.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	return &Layout{stateRoot: state, formalRoot: formal, projectID: projectID, stateIdentity: identity, projectIdentity: projectIdentity}, nil
}

func (l *Layout) Paths(id string) (Paths, error) {
	if l == nil || !ValidID(id) {
		return Paths{}, ErrOwnership
	}
	if err := l.stateIdentity.Revalidate(); err != nil {
		return Paths{}, err
	}
	if err := l.projectIdentity.Revalidate(); err != nil {
		return Paths{}, err
	}
	if err := validateAncestors(l.projectRoot(), true); err != nil {
		return Paths{}, err
	}
	root := filepath.Join(l.projectRoot(), id)
	return Paths{Root: root, FormalRoot: l.formalRoot, Baseline: filepath.Join(root, "baseline"), Repository: filepath.Join(root, "repo.git"), Checkout: filepath.Join(root, "checkout"), Run: filepath.Join(root, "run"), Journal: filepath.Join(l.projectRoot(), ".journals", id+".json")}, nil
}

func (l *Layout) projectRoot() string { return filepath.Join(l.stateRoot, l.projectID) }
func (l *Layout) ProjectID() string   { return l.projectID }
func (l *Layout) FormalRoot() string  { return l.formalRoot }

func (l *Layout) Close() error {
	if l == nil {
		return nil
	}
	return errors.Join(l.stateIdentity.Close(), l.projectIdentity.Close())
}

// Allocate never adopts an existing path, even when its label or mtime looks
// like a workspace. The caller must persist ownership before creating contents.
func (l *Layout) Allocate(id string) (Paths, error) {
	paths, err := l.Paths(id)
	if err != nil {
		return Paths{}, err
	}
	root, info, err := openVerifiedRoot(l.projectRoot())
	if err != nil {
		return Paths{}, err
	}
	defer root.Close()
	if err := l.projectIdentity.Revalidate(); err != nil {
		return Paths{}, err
	}
	current, err := os.Lstat(l.projectRoot())
	if err != nil || !os.SameFile(info, current) {
		return Paths{}, ErrOwnership
	}
	if err := root.Mkdir(id, 0700); err != nil {
		return Paths{}, err
	}
	if err := validateAncestors(paths.Root, true); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

func contains(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func validateAncestors(path string, private bool) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if private {
		ok, err := secfile.IsPrivatePath(path)
		if err != nil {
			return err
		}
		if !ok {
			return ErrOwnership
		}
	}
	return nil
}

// ValidateRoot verifies existing roots; it does not chmod user directories.
func ValidateRoot(path string, private bool) error { return validateAncestors(path, private) }

func makePrivateDirectory(path string) error {
	var missing []string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			if err := validateAncestors(current, false); err != nil {
				return err
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		if filepath.Dir(current) == current {
			return err
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		parent, identity, err := openVerifiedRoot(filepath.Dir(missing[i]))
		if err != nil {
			return err
		}
		err = parent.Mkdir(filepath.Base(missing[i]), 0700)
		parent.Close()
		if err != nil {
			return err
		}
		if err := revalidateRoot(filepath.Dir(missing[i]), identity); err != nil {
			return err
		}
	}
	return validateAncestors(path, true)
}
