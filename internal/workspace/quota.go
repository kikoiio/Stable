package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"
)

// Budget reserves service-controlled file writes. It is not a hard disk quota
// for commands, and must not be used to claim sandbox command enforcement.
type Budget struct {
	mu             sync.Mutex
	limits         Limits
	items          map[string]*allocation
	used, reserved int64
}

type allocation struct {
	used, reserved int64
	pending        bool
	reservations   int
}

type Reservation struct {
	budget          *Budget
	id              string
	bytes           int64
	create, settled bool
}

type Usage struct {
	Workspaces               int
	UsedBytes, ReservedBytes int64
}

func NewBudget(limits Limits) *Budget {
	return &Budget{limits: limits.Normalized(), items: make(map[string]*allocation)}
}

// Restore retains every live resource, including kept/interrupted records.
// Over-limit restored resources stay charged and block further reservations.
// Callers must still reconcile actual disk usage with persistent facts.
func (b *Budget) Restore(records []Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, item := range b.items {
		if item.reservations != 0 {
			return errors.New("workspace budget has active reservations")
		}
	}
	items := make(map[string]*allocation)
	var used int64
	over := false
	for _, record := range records {
		if record.Snapshot.State == StateRemoved {
			continue
		}
		id := record.Snapshot.ID
		if !ValidID(id) || record.UsedBytes < 0 || items[id] != nil || record.UsedBytes > int64(^uint64(0)>>1)-used {
			return ErrOwnership
		}
		items[id] = &allocation{used: record.UsedBytes}
		used += record.UsedBytes
		over = over || record.UsedBytes > b.limits.MaxWorkspaceBytes
	}
	b.items, b.used, b.reserved = items, used, 0
	if over || len(items) > b.limits.MaxWorkspaces || used > b.limits.MaxProjectBytes {
		return ErrQuota
	}
	return nil
}

func (b *Budget) ReserveCreate(id string, bytes int64) (*Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !ValidID(id) || bytes < 0 {
		return nil, ErrOwnership
	}
	if b.items[id] != nil {
		return nil, os.ErrExist
	}
	if len(b.items) >= b.limits.MaxWorkspaces || bytes > b.limits.MaxWorkspaceBytes || bytes > b.limits.MaxProjectBytes-b.used-b.reserved {
		return nil, ErrQuota
	}
	b.items[id] = &allocation{pending: true, reserved: bytes, reservations: 1}
	b.reserved += bytes
	return &Reservation{budget: b, id: id, bytes: bytes, create: true}, nil
}

// ReserveWrite reserves positive growth before writing; Commit takes the
// actual signed byte delta, so shrinking/deleting files releases usage too.
func (b *Budget) ReserveWrite(id string, growth int64) (*Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.items[id]
	if item == nil || item.pending || growth < 0 {
		return nil, ErrOwnership
	}
	if growth > 0 && (growth > b.limits.MaxWorkspaceBytes-item.used-item.reserved || growth > b.limits.MaxProjectBytes-b.used-b.reserved) {
		return nil, ErrQuota
	}
	item.reserved += growth
	item.reservations++
	b.reserved += growth
	return &Reservation{budget: b, id: id, bytes: growth}, nil
}

func (r *Reservation) Commit(delta int64) error {
	if r == nil || r.budget == nil {
		return ErrOwnership
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.settled {
		return nil
	}
	item := b.items[r.id]
	if item == nil || delta > r.bytes || delta < -item.used {
		return ErrQuota
	}
	item.used += delta
	item.reserved -= r.bytes
	item.reservations--
	item.pending = false
	b.used += delta
	b.reserved -= r.bytes
	r.settled = true
	return nil
}

func (r *Reservation) Release() {
	if r == nil || r.budget == nil {
		return
	}
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.settled {
		return
	}
	if item := b.items[r.id]; item != nil {
		item.reserved -= r.bytes
		item.reservations--
		if r.create {
			delete(b.items, r.id)
		}
	}
	b.reserved -= r.bytes
	r.settled = true
}

// Remove is called after a confirmed owned-root removal, never on a name scan.
func (b *Budget) Remove(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := b.items[id]
	if item == nil {
		return nil
	}
	if item.reservations != 0 {
		return errors.New("workspace budget has active reservations")
	}
	b.used -= item.used
	delete(b.items, id)
	return nil
}

func (b *Budget) Usage() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Usage{Workspaces: len(b.items), UsedBytes: b.used, ReservedBytes: b.reserved}
}

// DiskUsage includes baseline, checkout, private Git and run data. The scan
// counts allocated blocks (including directory metadata) or logical bytes,
// whichever is greater, and rejects links/special files. It does not enforce
// command writes while the scan is in progress.
func DiskUsage(ctx context.Context, path string, limits Limits) (int64, error) {
	limits = limits.Normalized()
	root, identity, err := openVerifiedRoot(path)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	used := int64(0)
	add := func(info os.FileInfo) error {
		bytes, err := allocatedBytes(info)
		if err != nil {
			return err
		}
		if bytes > limits.MaxWorkspaceBytes-used {
			return ErrQuota
		}
		used += bytes
		return nil
	}
	if err := add(identity); err != nil {
		return used, err
	}
	err = walkBounded(ctx, root, limits.MaxEntries, func(name string, entry fs.DirEntry, _ error) error {
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
		if info.Mode().IsRegular() {
			if err := validateHardlinks(info); err != nil {
				return err
			}
		}
		return add(info)
	})
	if err == nil {
		err = revalidateRoot(path, identity)
	}
	return used, err
}
