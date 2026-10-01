package dependency

import (
	"context"
	"fmt"
	"path/filepath"

	"stable/internal/core"
	"stable/internal/execution"
	"stable/internal/goalrun"
)

type State interface {
	GetGoalSnapshot(context.Context, string) (core.GoalSnapshot, error)
	ReconcileDependencies(context.Context, string, []core.DependencySnapshot) (core.DependencyRefresh, error)
	SetEventStatus(context.Context, string, string) error
}

type WakeFunc func(context.Context, string, string) error

type Refresher struct {
	State     State
	Collector core.DependencyCollector
	Wake      WakeFunc
}

// NewKiCadRefresher builds the single shared dependency rule using the same
// Python capability bridge and goal wake path as the worker runtime.
func NewKiCadRefresher(state State, runRoot, projectRoot, temporalAddress string) (*Refresher, error) {
	project, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(runRoot)
	if err != nil {
		return nil, err
	}
	bridge := &execution.PythonBridge{Script: filepath.Join(project, "workers/kicad/bridge.py"), AllowedRoot: root}
	return &Refresher{State: state, Collector: KiCadCollector{Kicad: bridge}, Wake: func(ctx context.Context, goalID, eventID string) error {
		return goalrun.WakeGoal(ctx, temporalAddress, goalID, eventID)
	}}, nil
}

func (r *Refresher) Refresh(ctx context.Context, goalID string) (core.DependencyRefresh, error) {
	if r == nil || r.State == nil || r.Collector == nil {
		return core.DependencyRefresh{}, fmt.Errorf("dependency refresher is not configured")
	}
	current, err := r.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	dependencies, err := r.Collector.Collect(ctx, current.Goal)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	result, err := r.State.ReconcileDependencies(ctx, goalID, dependencies)
	if err != nil {
		return core.DependencyRefresh{}, err
	}
	if result.Event == nil || r.Wake == nil {
		return result, nil
	}
	if err = r.Wake(ctx, goalID, result.Event.ID); err != nil {
		result.WakeError = err.Error()
		return result, nil
	}
	if err = r.State.SetEventStatus(ctx, result.Event.ID, "signaled"); err != nil {
		return result, err
	}
	result.Event.Status = "signaled"
	result.Snapshot, err = r.State.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return result, err
	}
	return result, nil
}
