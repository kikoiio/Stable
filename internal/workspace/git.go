package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"stable/internal/platform/proc"
	"stable/internal/platform/secfile"
)

const privateGitVersion = 1
const gitStateName = "git-state.json"
const privateGitConfiguration = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n\tlogallrefupdates = false\n"
const baselineRef = "refs/stable/baseline"
const checkoutRef = "refs/heads/stable"
const gitOutputLimit = 64 << 10

// GitState is a service-private materialization receipt. It is stored beside
// the resource journal, never in a public task snapshot or a model argument.
type GitState struct {
	Version              int          `json:"version"`
	WorkspaceID          string       `json:"workspace_id"`
	Generation           uint64       `json:"generation"`
	BaselineDigest       string       `json:"baseline_digest"`
	BaselineCommit       string       `json:"baseline_commit"`
	BaselineIdentity     RootIdentity `json:"baseline_identity"`
	RepositoryIdentity   RootIdentity `json:"repository_identity"`
	CheckoutIdentity     RootIdentity `json:"checkout_identity"`
	GitDirectoryIdentity RootIdentity `json:"git_directory_identity"`
	RunIdentity          RootIdentity `json:"run_identity"`
	UsedBytes            int64        `json:"used_bytes"`
}

type gitInvocation struct {
	Paths      Paths
	GitDir     string
	WorkTree   string
	Args       []string
	Stdin      io.Reader
	Initialize bool
}

// PrivateGit only executes service-selected plumbing against this item's
// private repository. It cannot fetch, open the formal repository, or execute
// project commands, hooks, attributes, credential helpers or model argv.
type PrivateGit struct {
	layout *Layout
	store  *OwnershipStore
	limits Limits
	binary string
	runGit func(context.Context, gitInvocation) ([]byte, error)
	mu     sync.Mutex
}

func NewPrivateGit(layout *Layout, store *OwnershipStore, limits Limits) (*PrivateGit, error) {
	if layout == nil || store == nil || store.layout != layout {
		return nil, ErrOwnership
	}
	// A fixed system executable avoids resolving a project-controlled PATH.
	binary := "/usr/bin/git"
	if err := validateAncestors(filepath.Dir(binary), false); err != nil {
		return nil, fmt.Errorf("%w: system Git directory", ErrUnavailable)
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return nil, fmt.Errorf("%w: system Git executable", ErrUnavailable)
	}
	g := &PrivateGit{layout: layout, store: store, limits: limits.Normalized(), binary: binary}
	g.runGit = g.execute
	return g, nil
}

// Materialize requires a persisted, physically owned creating intent. Its
// receipt is returned only after rechecking source data and all private roots.
// The manager is responsible for the public ready event/outcome and project
// budget reservation. On failure the journal and outer root remain owned.
func (g *PrivateGit) Materialize(ctx context.Context, scope Scope, id string) (result GitState, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, g.limits.MaxDuration)
	defer cancel()
	record, err := g.store.Load(ctx, scope, id)
	if err != nil {
		return GitState{}, err
	}
	if record.Snapshot.State != StateCreating || record.Operation.Phase != "intent" {
		return GitState{}, ErrOwnership
	}
	paths, err := g.layout.Paths(id)
	if err != nil {
		return GitState{}, err
	}
	root, rootInfo, err := openVerifiedRoot(paths.Root)
	if err != nil {
		return GitState{}, err
	}
	defer root.Close()
	for _, name := range []string{"baseline", "repo.git", "checkout", "run", gitStateName} {
		if _, err := root.Lstat(name); err == nil {
			return GitState{}, os.ErrExist
		} else if !errors.Is(err, os.ErrNotExist) {
			return GitState{}, err
		}
	}
	dir, err := root.Open(".")
	if err != nil {
		return GitState{}, err
	}
	entries, readErr := dir.ReadDir(1)
	dir.Close()
	if len(entries) != 0 || readErr != nil && !errors.Is(readErr, io.EOF) {
		return GitState{}, ErrOwnership
	}
	created := make(map[string]os.FileInfo)
	success := false
	defer func() {
		if success {
			return
		}
		// Roll back only physical entities captured when this operation created
		// them. Replacement or an unknown partial root is preserved, not adopted.
		if cleanupErr := g.rollback(scope, id, root, rootInfo, created); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("private Git rollback blocked: %w", cleanupErr))
		}
	}()
	allocate := func(name string) error {
		if err := root.Mkdir(name, 0700); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err == nil {
			created[name] = info
		}
		return err
	}
	if err := allocate("run"); err != nil {
		return GitState{}, err
	}
	for _, name := range []string{"home", "tmp", "empty-hooks"} {
		if err := root.Mkdir(filepath.Join("run", name), 0700); err != nil {
			return GitState{}, err
		}
	}
	sourceHandle, sourceIdentity, err := openVerifiedRoot(g.layout.FormalRoot())
	if err != nil {
		return GitState{}, err
	}
	defer sourceHandle.Close()
	expected, err := BuildManifest(ctx, g.layout.FormalRoot(), g.limits)
	if err != nil {
		return GitState{}, err
	}
	if err := g.checkEstimatedUsage(root, expected); err != nil {
		return GitState{}, err
	}
	baseline, err := copySnapshot(ctx, g.layout.FormalRoot(), paths.Baseline, g.limits, func(info os.FileInfo) error {
		created["baseline"] = info
		return nil
	})
	if err != nil {
		return GitState{}, err
	}
	if baseline.Digest != expected.Digest {
		return GitState{}, ErrSourceChanged
	}
	if err := allocate("repo.git"); err != nil {
		return GitState{}, err
	}
	if _, err := g.runGit(ctx, gitInvocation{Paths: paths, GitDir: paths.Repository, Initialize: true, Args: []string{"init", "--bare", "--template=", "--object-format=sha1", "--initial-branch=stable", "--", paths.Repository}}); err != nil {
		return GitState{}, err
	}
	if err := g.makeRepositoryPrivate(ctx, paths.Repository); err != nil {
		return GitState{}, err
	}
	repository, _, err := openVerifiedRoot(paths.Repository)
	if err != nil {
		return GitState{}, err
	}
	defer repository.Close()
	if err := replacePrivateFile(repository, "config", []byte(privateGitConfiguration)); err != nil {
		return GitState{}, err
	}
	if err := g.importBaseline(ctx, paths, baseline); err != nil {
		return GitState{}, err
	}
	commitBytes, err := g.runGit(ctx, gitInvocation{Paths: paths, GitDir: paths.Repository, Args: []string{"rev-parse", "--verify", checkoutRef + "^{commit}"}})
	if err != nil {
		return GitState{}, err
	}
	commit := strings.TrimSpace(string(commitBytes))
	if !validGitOID(commit) {
		return GitState{}, ErrOwnership
	}
	if _, err := g.runGit(ctx, gitInvocation{Paths: paths, GitDir: paths.Repository, Args: []string{"update-ref", baselineRef, commit}}); err != nil {
		return GitState{}, err
	}
	if _, err := DiskUsage(ctx, paths.Root, g.limits); err != nil {
		return GitState{}, err
	}
	checkout, err := copySnapshot(ctx, paths.Baseline, paths.Checkout, g.limits, func(info os.FileInfo) error {
		created["checkout"] = info
		return nil
	})
	if err != nil {
		return GitState{}, err
	}
	if checkout.Digest != baseline.Digest {
		return GitState{}, ErrSourceChanged
	}
	gitDirRelative := filepath.Join("worktrees", id)
	if err := repository.MkdirAll(gitDirRelative, 0700); err != nil {
		return GitState{}, err
	}
	gitDir := filepath.Join(paths.Repository, gitDirRelative)
	for name, data := range map[string]string{
		filepath.Join(gitDirRelative, "HEAD"):      commit + "\n",
		filepath.Join(gitDirRelative, "gitdir"):    paths.Checkout + "/.git\n",
		filepath.Join(gitDirRelative, "commondir"): "../../\n",
		filepath.Join(gitDirRelative, "locked"):    "Stable service managed\n",
	} {
		if err := createPrivateFile(repository, name, []byte(data)); err != nil {
			return GitState{}, err
		}
	}
	checkoutRoot, _, err := openVerifiedRoot(paths.Checkout)
	if err != nil {
		return GitState{}, err
	}
	defer checkoutRoot.Close()
	if err := createPrivateFile(checkoutRoot, ".git", []byte("gitdir: "+gitDir+"\n")); err != nil {
		return GitState{}, err
	}
	// read-tree initializes only the private linked index; no checkout/filter
	// operation ever interprets source .gitattributes or runs project code.
	if _, err := g.runGit(ctx, gitInvocation{Paths: paths, GitDir: gitDir, WorkTree: paths.Checkout, Args: []string{"read-tree", "--reset", commit}}); err != nil {
		return GitState{}, err
	}
	if err := g.makeRepositoryPrivate(ctx, paths.Repository); err != nil {
		return GitState{}, err
	}
	after, err := BuildManifest(ctx, g.layout.FormalRoot(), g.limits)
	if err != nil {
		return GitState{}, err
	}
	if after.Digest != baseline.Digest {
		return GitState{}, ErrSourceChanged
	}
	if err := revalidateRoot(g.layout.FormalRoot(), sourceIdentity); err != nil {
		return GitState{}, err
	}
	result = GitState{Version: privateGitVersion, WorkspaceID: id, Generation: record.Snapshot.Generation, BaselineDigest: baseline.Digest, BaselineCommit: commit}
	identities := []struct {
		path     string
		identity *RootIdentity
	}{
		{paths.Baseline, &result.BaselineIdentity},
		{paths.Repository, &result.RepositoryIdentity}, {paths.Checkout, &result.CheckoutIdentity},
		{gitDir, &result.GitDirectoryIdentity}, {paths.Run, &result.RunIdentity},
	}
	for _, item := range identities {
		info, err := os.Lstat(item.path)
		if err != nil {
			return GitState{}, err
		}
		*item.identity, err = rootIdentity(info)
		if err != nil {
			return GitState{}, err
		}
	}
	result.UsedBytes, err = DiskUsage(ctx, paths.Root, g.limits)
	if err != nil {
		return GitState{}, err
	}
	if err := g.validateContents(ctx, paths, result); err != nil {
		return GitState{}, err
	}
	if err := revalidateRoot(paths.Root, rootInfo); err != nil {
		return GitState{}, err
	}
	if _, err := g.store.Load(ctx, scope, id); err != nil {
		return GitState{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return GitState{}, err
	}
	info, err := createAtomicReceipt(root, gitStateName, raw)
	if info != nil {
		created[gitStateName] = info
	}
	if err != nil {
		return GitState{}, err
	}
	if err := syncDirectory(root); err != nil {
		return GitState{}, err
	}
	result.UsedBytes, err = DiskUsage(ctx, paths.Root, g.limits)
	if err != nil {
		return GitState{}, err
	}
	success = true
	return result, nil
}

func validGitOID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 20 && strings.ToLower(value) == value
}

func (g *PrivateGit) rollback(scope Scope, id string, root *os.Root, identity os.FileInfo, created map[string]os.FileInfo) error {
	if _, err := g.store.Load(context.Background(), scope, id); err != nil {
		return err
	}
	paths, err := g.layout.Paths(id)
	if err != nil {
		return err
	}
	if err := revalidateRoot(paths.Root, identity); err != nil {
		return err
	}
	var errs []error
	for _, name := range []string{gitStateName, "checkout", "repo.git", "baseline", "run"} {
		current, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || created[name] == nil || !os.SameFile(created[name], current) || current.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, ErrOwnership)
			continue
		}
		if err := root.RemoveAll(name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := syncDirectory(root); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func createPrivateFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func createAtomicReceipt(root *os.Root, name string, data []byte) (os.FileInfo, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	temp := ".git-receipt-" + id
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	defer func() {
		file.Close()
		if info, err := root.Lstat(temp); err == nil && os.SameFile(identity, info) {
			root.Remove(temp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return nil, ErrOwnership
	}
	if err := root.Rename(temp, name); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return identity, err
	}
	return identity, nil
}

func replacePrivateFile(root *os.Root, name string, data []byte) error {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || validateHardlinks(info) != nil {
		return ErrOwnership
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	return createPrivateFile(root, name, data)
}

func syncDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (g *PrivateGit) makeRepositoryPrivate(ctx context.Context, path string) error {
	root, identity, err := openVerifiedRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	err = walkBounded(ctx, root, g.limits.MaxEntries, func(name string, _ fs.DirEntry, _ error) error {
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		if info.IsDir() {
			return secfile.ChmodRoot(root, name, 0700)
		}
		if err := validateHardlinks(info); err != nil {
			return err
		}
		return secfile.ChmodRoot(root, name, 0600)
	})
	if err != nil {
		return err
	}
	return revalidateRoot(path, identity)
}

func (g *PrivateGit) checkEstimatedUsage(root *os.Root, manifest Manifest) error {
	unit, err := allocationUnit(root)
	if err != nil {
		return err
	}
	// Reserve room for two independently copied data trees, packed objects,
	// indexes and directory growth. This deliberately errs on the high side.
	dirs := map[string]struct{}{".": {}}
	var data int64
	for _, entry := range manifest.Entries {
		data += ((entry.Size+unit-1)/unit)*unit + 512
		for dir := filepath.Dir(entry.Path); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = struct{}{}
		}
	}
	estimate := 2*(data+int64(len(dirs))*unit) + manifest.Bytes + int64(len(manifest.Entries))*1024 + 16<<20
	if estimate > g.limits.MaxWorkspaceBytes {
		return ErrQuota
	}
	return nil
}

func (g *PrivateGit) importBaseline(ctx context.Context, paths Paths, manifest Manifest) error {
	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := writeImport(ctx, writer, paths.Baseline, manifest)
		writer.CloseWithError(err)
		produced <- err
	}()
	_, runErr := g.runGit(ctx, gitInvocation{Paths: paths, GitDir: paths.Repository, Args: []string{"fast-import", "--quiet", "--date-format=raw"}, Stdin: reader})
	reader.CloseWithError(runErr)
	producerErr := <-produced
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(runErr, producerErr)
}

func writeImport(ctx context.Context, writer io.Writer, baseline string, manifest Manifest) error {
	root, identity, err := openVerifiedRoot(baseline)
	if err != nil {
		return err
	}
	defer root.Close()
	entries := append([]ManifestEntry(nil), manifest.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(entry.Path)
		if err != nil {
			return err
		}
		file, err := openRegular(root, entry.Path, info)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, "blob\nmark :%d\ndata %d\n", i+1, entry.Size); err != nil {
			file.Close()
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(writer, hash), io.LimitReader(contextReader{ctx, file}, entry.Size+1))
		file.Close()
		if copyErr != nil {
			return copyErr
		}
		if written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Digest || uint32(info.Mode().Perm()) != entry.Mode {
			return ErrSourceChanged
		}
		if _, err := io.WriteString(writer, "\n"); err != nil {
			return err
		}
	}
	message := "Stable workspace baseline\n"
	if _, err := fmt.Fprintf(writer, "commit %s\ncommitter Stable Workspace <workspace@stable.invalid> 0 +0000\ndata %d\n%s", checkoutRef, len(message), message); err != nil {
		return err
	}
	for i, entry := range entries {
		mode := "100644"
		if entry.Mode&0111 != 0 {
			mode = "100755"
		}
		// Fast-import accepts Git's C-style quoting, including escaped newline.
		if _, err := fmt.Fprintf(writer, "M %s :%d %s\n", mode, i+1, quoteGitPath(entry.Path)); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, "\ndone\n"); err != nil {
		return err
	}
	return revalidateRoot(baseline, identity)
}

func quoteGitPath(path string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	for _, b := range []byte(path) {
		switch b {
		case '\\', '"':
			quoted.WriteByte('\\')
			quoted.WriteByte(b)
		default:
			if b < 0x20 || b >= 0x7f {
				quoted.WriteByte('\\')
				quoted.WriteString(fmt.Sprintf("%03o", b))
			} else {
				quoted.WriteByte(b)
			}
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}

func sanitizedGitEnvironment(paths Paths) []string {
	return []string{
		"PATH=/usr/bin:/bin", "LC_ALL=C", "LANG=C", "TZ=UTC",
		"HOME=" + filepath.Join(paths.Run, "home"), "XDG_CONFIG_HOME=" + filepath.Join(paths.Run, "home"),
		"TMPDIR=" + filepath.Join(paths.Run, "tmp"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=", "GIT_PROTOCOL_FROM_USER=0", "GIT_OPTIONAL_LOCKS=0",
		"GIT_LITERAL_PATHSPECS=1", "GIT_PAGER=cat",
	}
}

func gitArguments(invocation gitInvocation) []string {
	args := []string{"--no-pager", "--no-optional-locks"}
	for _, setting := range []string{
		"core.hooksPath=" + filepath.Join(invocation.Paths.Run, "empty-hooks"),
		"protocol.allow=never", "protocol.file.allow=never", "credential.helper=",
		"core.fsmonitor=false", "core.untrackedCache=false", "core.autocrlf=false",
		"core.safecrlf=false", "core.attributesFile=/dev/null", "commit.gpgSign=false",
		"tag.gpgSign=false", "maintenance.auto=false", "gc.auto=0", "init.templateDir=",
		"submodule.recurse=false", "core.sparseCheckout=false", "index.sparse=false",
		"core.splitIndex=false", "core.preloadIndex=false",
	} {
		args = append(args, "-c", setting)
	}
	if invocation.GitDir != "" {
		args = append(args, "--git-dir="+invocation.GitDir)
	}
	if invocation.WorkTree != "" {
		args = append(args, "--work-tree="+invocation.WorkTree)
	}
	return append(args, invocation.Args...)
}

func (g *PrivateGit) execute(ctx context.Context, invocation gitInvocation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(invocation.Args) == 0 {
		return nil, ErrOwnership
	}
	if !invocation.Initialize {
		if err := validateRepositoryConfiguration(invocation.Paths.Repository); err != nil {
			return nil, err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &gitOutput{limit: gitOutputLimit, cancel: cancel}
	command := exec.CommandContext(runCtx, g.binary, gitArguments(invocation)...)
	command.Dir = invocation.Paths.Root
	command.Env = sanitizedGitEnvironment(invocation.Paths)
	command.Stdin = invocation.Stdin
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = time.Second
	if err := proc.ConfigureChild(command); err != nil {
		return nil, fmt.Errorf("%w: private Git process isolation", ErrUnavailable)
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return proc.KillGroup(command.Process.Pid, true)
	}
	err := command.Run()
	if output.exceeded {
		return nil, ErrQuota
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("private Git %s failed: %w", invocation.Args[0], err)
	}
	return output.buffer.Bytes(), nil
}

type gitOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (w *gitOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(data) > w.limit-w.buffer.Len() {
		w.exceeded = true
		w.cancel()
		return 0, ErrQuota
	}
	return w.buffer.Write(data)
}

// Read a small service-owned regular file without symlinks or hardlinks.
func readPrivateFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateFile(info, filepath.Join(root.Name(), name)); err != nil || info.Size() > limit {
		return nil, ErrOwnership
	}
	file, err := openRegular(root, name, info)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, ErrQuota
	}
	return data, err
}

func validateRepositoryConfiguration(path string) error {
	root, _, err := openVerifiedRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	raw, err := readPrivateFile(root, "config", 4096)
	if err != nil {
		return err
	}
	if string(raw) != privateGitConfiguration {
		return ErrOwnership
	}
	return nil
}

// Validate can reconcile a complete receipt after a crash, without restarting
// Git initialization or a model. Checkout data may be dirty; metadata and the
// immutable baseline must still match this physically owned resource.
func (g *PrivateGit) Validate(ctx context.Context, scope Scope, id string) (GitState, error) {
	record, err := g.store.Load(ctx, scope, id)
	if err != nil {
		return GitState{}, err
	}
	paths, err := g.layout.Paths(id)
	if err != nil {
		return GitState{}, err
	}
	root, _, err := openVerifiedRoot(paths.Root)
	if err != nil {
		return GitState{}, err
	}
	defer root.Close()
	raw, err := readPrivateFile(root, gitStateName, 4096)
	if err != nil {
		return GitState{}, err
	}
	var state GitState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return GitState{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return GitState{}, ErrOwnership
	}
	if state.Version != privateGitVersion || state.WorkspaceID != id || state.Generation == 0 || state.Generation > record.Snapshot.Generation || !validGitOID(state.BaselineCommit) || state.UsedBytes < 0 {
		return GitState{}, ErrOwnership
	}
	if record.Snapshot.BaselineDigest != "" && record.Snapshot.BaselineDigest != state.BaselineDigest {
		return GitState{}, ErrOwnership
	}
	if err := g.validateContents(ctx, paths, state); err != nil {
		return GitState{}, err
	}
	state.UsedBytes, err = DiskUsage(ctx, paths.Root, g.limits)
	return state, err
}

func (g *PrivateGit) validateContents(ctx context.Context, paths Paths, state GitState) error {
	gitDirRelative := filepath.Join("worktrees", state.WorkspaceID)
	gitDir := filepath.Join(paths.Repository, gitDirRelative)
	for _, item := range []struct {
		path     string
		identity RootIdentity
	}{
		{paths.Baseline, state.BaselineIdentity}, {paths.Repository, state.RepositoryIdentity},
		{paths.Checkout, state.CheckoutIdentity}, {gitDir, state.GitDirectoryIdentity}, {paths.Run, state.RunIdentity},
	} {
		if err := validateAncestors(item.path, true); err != nil {
			return err
		}
		info, err := os.Lstat(item.path)
		if err != nil {
			return err
		}
		identity, err := rootIdentity(info)
		if err != nil || identity != item.identity {
			return ErrOwnership
		}
	}
	baseline, err := BuildManifest(ctx, paths.Baseline, g.limits)
	if err != nil {
		return err
	}
	if baseline.Digest != state.BaselineDigest {
		return ErrSourceChanged
	}
	baselineRoot, _, err := openVerifiedRoot(paths.Baseline)
	if err != nil {
		return err
	}
	defer baselineRoot.Close()
	if err := rejectProtectedRoots(baselineRoot, false); err != nil {
		return err
	}
	checkoutRoot, _, err := openVerifiedRoot(paths.Checkout)
	if err != nil {
		return err
	}
	defer checkoutRoot.Close()
	if err := rejectProtectedRoots(checkoutRoot, true); err != nil {
		return err
	}
	pointer, err := readPrivateFile(checkoutRoot, ".git", 4096)
	if err != nil || string(pointer) != "gitdir: "+gitDir+"\n" {
		return ErrOwnership
	}
	if err := validateRepositoryConfiguration(paths.Repository); err != nil {
		return err
	}
	repository, identity, err := openVerifiedRoot(paths.Repository)
	if err != nil {
		return err
	}
	defer repository.Close()
	for name, want := range map[string]string{
		"HEAD":                                  "ref: " + checkoutRef + "\n",
		checkoutRef:                             state.BaselineCommit + "\n",
		baselineRef:                             state.BaselineCommit + "\n",
		filepath.Join(gitDirRelative, "HEAD"):   state.BaselineCommit + "\n",
		filepath.Join(gitDirRelative, "gitdir"): paths.Checkout + "/.git\n",
		filepath.Join(gitDirRelative, "commondir"): "../../\n",
		filepath.Join(gitDirRelative, "locked"):    "Stable service managed\n",
	} {
		got, err := readPrivateFile(repository, name, 4096)
		if err != nil || string(got) != want {
			return ErrOwnership
		}
	}
	err = walkBounded(ctx, repository, g.limits.MaxEntries, func(name string, _ fs.DirEntry, _ error) error {
		info, err := repository.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		if ok, err := secfile.IsPrivatePath(filepath.Join(paths.Repository, name)); err != nil || !ok {
			return ErrOwnership
		}
		if info.Mode().IsRegular() && validateHardlinks(info) != nil {
			return ErrUnsafePath
		}
		for _, blocked := range []string{"objects/info/alternates", "objects/info/http-alternates", "info/grafts", "info/attributes", "refs/replace", "refs/remotes", "config.worktree", "shallow", "packed-refs"} {
			if name == blocked || strings.HasPrefix(name, blocked+"/") {
				return ErrUnsafePath
			}
		}
		if strings.HasPrefix(name, "hooks/") && !info.IsDir() {
			return ErrUnsafePath
		}
		if strings.HasPrefix(name, "refs/") && !info.IsDir() && name != checkoutRef && name != baselineRef {
			return ErrOwnership
		}
		if strings.HasPrefix(name, "worktrees/") && name != gitDirRelative && !strings.HasPrefix(name, gitDirRelative+"/") {
			return ErrOwnership
		}
		return nil
	})
	if err != nil {
		return err
	}
	return revalidateRoot(paths.Repository, identity)
}

func rejectProtectedRoots(root *os.Root, allowGitPointer bool) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(64)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if ProtectedRoot(entry.Name()) && !(allowGitPointer && entry.Name() == ".git") {
				return ErrUnsafePath
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
