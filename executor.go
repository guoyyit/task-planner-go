package taskplan

import (
	"context"
	"sort"
	"sync"
)

// Runner executes one primitive action against the real world. It
// receives the action and the state as committed when the action's
// layer started.
//
// Run returns the action's actual effects. Returning nil effects means
// "exactly the declared effects". Returning different effects is legal
// and triggers a replan from the observed state. A non-nil error marks
// the action as failed; actual effects may still be reported.
type Runner interface {
	Run(ctx context.Context, action Action, state State) (*Effects, error)
}

// RunnerFunc adapts a plain function to the Runner interface.
type RunnerFunc func(ctx context.Context, action Action, state State) (*Effects, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, action Action, state State) (*Effects, error) {
	return f(ctx, action, state)
}

// ExecutorConfig tunes the executor. The zero value is usable: strictly
// sequential execution, no persistence, default replan budget.
type ExecutorConfig struct {
	// MaxParallel caps how many actions of one layer run concurrently.
	// Values <= 1 mean sequential execution. The value only limits
	// concurrency; it never changes the committed outcome.
	MaxParallel int `json:"maxParallel,omitempty"`
	// MaxReplans bounds how often the executor replans after action
	// failures before giving up with FailureExecutionFailed.
	// Zero means DefaultMaxReplans.
	MaxReplans int `json:"maxReplans,omitempty"`
	// StorePath, when non-empty, enables progress persistence: after
	// every committed action the executor atomically writes a snapshot
	// that a later Run call with the same path resumes from.
	StorePath string `json:"storePath,omitempty"`
}

// DefaultMaxReplans is the replan budget used when
// ExecutorConfig.MaxReplans is zero.
const DefaultMaxReplans = 8

func (c ExecutorConfig) maxReplans() int {
	if c.MaxReplans <= 0 {
		return DefaultMaxReplans
	}
	return c.MaxReplans
}

// ActionRecord describes one executed action occurrence.
type ActionRecord struct {
	Action string `json:"action"`
	Layer  int    `json:"layer"`
}

// Result summarizes a finished or failed executor run.
type Result struct {
	// FinalState is the committed world state when Run returned.
	FinalState State `json:"-"`
	// Executed lists every action that ran successfully, in commit order.
	Executed []ActionRecord `json:"executed"`
	// Replans counts how often the executor replanned.
	Replans int `json:"replans"`
}

// Executor drives plans produced by a Planner, monitors deviations and
// replans the remaining work from the observed state.
type Executor struct {
	planner *Planner
	cfg     ExecutorConfig
}

// NewExecutor returns an Executor using the given configurations.
func NewExecutor(cfg ExecutorConfig, pc PlannerConfig) *Executor {
	return &Executor{planner: NewPlanner(pc), cfg: cfg}
}

// Run executes the goals from the initial state (or resumes from the
// persisted snapshot when StorePath is configured and a snapshot
// exists). It returns a *Failure when the run cannot be completed; the
// Result is still returned for inspection in that case.
//
// Execution semantics:
//
//   - The plan's layers execute in order; every action of a layer is
//     always started (bounded by MaxParallel) and the executor waits for
//     all of them, even when a sibling fails. Results are then committed
//     deterministically in plan order. This makes the outcome of a
//     branch failure independent of goroutine timing.
//   - When an action fails or reports effects different from its
//     declared ones, the observed effects are applied and the remaining
//     tasks are replanned from the observed state. Tasks behind
//     already-executed actions are never replanned, so finished work is
//     never repeated.
//   - After every committed action the progress snapshot is persisted
//     when StorePath is configured, so a restarted process resumes at
//     the last committed position.
func (e *Executor) Run(ctx context.Context, domain *Domain, initial State, goals []TaskRef, runner Runner) (*Result, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if runner == nil {
		runner = RunnerFunc(func(context.Context, Action, State) (*Effects, error) { return nil, nil })
	}

	res := &Result{}
	state := initial.Clone()
	remaining := append([]TaskRef(nil), goals...)
	executedTotal := 0
	if e.cfg.StorePath != "" {
		snap, err := loadSnapshot(e.cfg.StorePath)
		if err != nil {
			return nil, err
		}
		if snap != nil {
			state = NewState(snap.State...)
			remaining = append([]TaskRef(nil), snap.Remaining...)
			executedTotal = snap.Executed
			if snap.Done {
				res.FinalState = state
				return res, nil
			}
		}
	}

	persist := func(done bool) error {
		if e.cfg.StorePath == "" {
			return nil
		}
		return saveSnapshot(e.cfg.StorePath, &snapshot{
			State:     state.Sorted(),
			Remaining: append([]TaskRef(nil), remaining...),
			Executed:  executedTotal,
			Done:      done,
		})
	}

	maxReplans := e.cfg.maxReplans()
	replans := 0

	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			res.FinalState = state
			return res, err
		}
		plan, err := e.planner.Plan(domain, state, remaining)
		if err != nil {
			res.FinalState = state
			res.Replans = replans
			return res, err
		}
		if len(plan.Steps) == 0 {
			remaining = nil
			break
		}

		failed := false
		for li, layer := range plan.Layers {
			steps := make([]Step, len(layer))
			actions := make([]Action, len(layer))
			for i, si := range layer {
				steps[i] = plan.Steps[si]
				actions[i] = *domain.Actions[steps[i].Action]
			}
			results := e.runLayer(ctx, actions, state, runner)

			executedIDs := make(map[uint64]bool, len(layer))
			failedStep := -1
			for i, step := range steps {
				r := results[i]
				declared := domain.Actions[step.Action].Eff
				switch {
				case r.err != nil:
					if r.actual != nil {
						state.Apply(*r.actual)
					}
					if failedStep == -1 {
						failedStep = i
					}
				case r.actual != nil && !effectsEqual(*r.actual, declared):
					state.Apply(*r.actual)
					if failedStep == -1 {
						failedStep = i
					}
				default:
					state.Apply(declared)
					executedIDs[step.TaskID] = true
					executedTotal++
					res.Executed = append(res.Executed, ActionRecord{Action: step.Action, Layer: li})
				}
				if failedStep == -1 {
					remaining = filterAgenda(step.Agenda[1:], executedIDs)
				} else {
					remaining = filterAgenda(steps[failedStep].Agenda, executedIDs)
				}
				if err := persist(false); err != nil {
					res.FinalState = state
					return res, err
				}
			}
			if failedStep >= 0 {
				failed = true
				break
			}
		}
		if !failed {
			continue
		}
		replans++
		res.Replans = replans
		if replans > maxReplans {
			res.FinalState = state
			return res, execf("replan budget %d exhausted after action failure", maxReplans)
		}
	}

	res.FinalState = state
	res.Replans = replans
	if err := persist(true); err != nil {
		return res, err
	}
	return res, nil
}

// actionResult is the observed outcome of one executed action.
type actionResult struct {
	actual *Effects
	err    error
}

// runLayer starts every action of the layer, capping concurrency at
// MaxParallel, and waits for all of them. Each runner receives a private
// copy of the layer-start state. Results are indexed by plan position,
// never by completion order, keeping the commit deterministic.
func (e *Executor) runLayer(ctx context.Context, actions []Action, state State, runner Runner) []actionResult {
	results := make([]actionResult, len(actions))
	maxPar := e.cfg.MaxParallel
	if maxPar <= 0 {
		maxPar = 1
	}
	sem := make(chan struct{}, maxPar)
	var wg sync.WaitGroup
	for i, a := range actions {
		wg.Add(1)
		go func(i int, a Action) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			eff, err := runner.Run(ctx, a, state.Clone())
			results[i] = actionResult{actual: eff, err: err}
		}(i, a)
	}
	wg.Wait()
	return results
}

// filterAgenda drops the tasks whose ids are in executed, yielding the
// work that still has to be done.
func filterAgenda(agenda []TaskRef, executed map[uint64]bool) []TaskRef {
	out := make([]TaskRef, 0, len(agenda))
	for _, t := range agenda {
		if !executed[t.ID] {
			out = append(out, t)
		}
	}
	return out
}

// effectsEqual reports whether two effect sets are identical.
func effectsEqual(a, b Effects) bool {
	return equalSet(a.Add, b.Add) && equalSet(a.Del, b.Del)
}

func equalSet(xs, ys []string) bool {
	if len(xs) != len(ys) {
		return false
	}
	sx := append([]string(nil), xs...)
	sy := append([]string(nil), ys...)
	sort.Strings(sx)
	sort.Strings(sy)
	for i := range sx {
		if sx[i] != sy[i] {
			return false
		}
	}
	return true
}
