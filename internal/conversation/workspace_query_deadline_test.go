package conversation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkspaceQueriesUseThirtySecondBoundAndPreserveParentDeadline(t *testing.T) {
	for _, op := range []string{"worktree_list", "worktree_get", "worktree_preview"} {
		t.Run(op+" default bound", func(t *testing.T) {
			started := time.Now()
			var captured time.Time
			_, err := withWorkspaceQuery(context.Background(), op, func(ctx context.Context) (struct{}, error) {
				captured, _ = ctx.Deadline()
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			remaining := time.Until(captured)
			if captured.IsZero() || remaining <= 0 || remaining > workspaceQueryTimeout || captured.After(started.Add(workspaceQueryTimeout+time.Millisecond)) {
				t.Fatalf("query deadline=%s remaining=%s, want <=%s from request start", captured, remaining, workspaceQueryTimeout)
			}
		})
	}

	t.Run("shorter parent deadline is preserved", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		want, _ := parent.Deadline()
		_, err := withWorkspaceQuery(parent, "worktree_get", func(ctx context.Context) (struct{}, error) {
			got, ok := ctx.Deadline()
			if !ok || !got.Equal(want) {
				t.Fatalf("query deadline=%s, want parent deadline %s", got, want)
			}
			return struct{}{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("timeout is returned without invoking post-timeout effects", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		var effects int
		_, err := withWorkspaceQuery(parent, "worktree_list", func(ctx context.Context) (struct{}, error) {
			<-ctx.Done()
			if err := ctx.Err(); err != nil {
				return struct{}{}, err
			}
			effects++
			return struct{}{}, nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("query timeout error=%v, want context deadline exceeded", err)
		}
		if effects != 0 {
			t.Fatalf("timed-out read produced %d side effects", effects)
		}
	})

	for _, op := range []string{"worktree_create", "worktree_exit", "worktree_discard_preview"} {
		t.Run(op+" is not capped as a query", func(t *testing.T) {
			_, err := withWorkspaceQuery(context.Background(), op, func(ctx context.Context) (struct{}, error) {
				if _, ok := ctx.Deadline(); ok {
					t.Fatal("lifecycle operation was capped by the query timeout")
				}
				return struct{}{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
