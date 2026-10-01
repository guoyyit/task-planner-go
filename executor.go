package taskplanner

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ActionResult 是动作处理器的执行结果。
type ActionResult struct {
	// ObservedAdd / ObservedDel 为实际观察到的效果；AsExpected=true 时按声明效果处理。
	ObservedAdd []Literal
	ObservedDel []Literal
	AsExpected  bool
}

// ActionHandler 由调用方提供，负责真正驱动一个动作。
type ActionHandler func(ctx context.Context, action *GroundAction) (*ActionResult, error)

const (
	PolicyStopOnError = "stop"
	PolicyContinue    = "continue"
)

// ActionRecord 是单个动作实例的执行记录。
type ActionRecord struct {
	ID     string
	Name   string
	Args   []string
	Status string
}

// ExecutionReport 是一次（含恢复与重规划的）执行的最终报告。
type ExecutionReport struct {
	PlanID     string
	Status     string
	Replans    int
	Executed   []ActionRecord
	FinalState *State
}

// Executor 按计划驱动执行，监控偏差并在需要时重新规划。
type Executor struct {
	planner *Planner
	handler ActionHandler
	config  Config
}

func NewExecutor(domain *Domain, config Config, handler ActionHandler) *Executor {
	cfg := normalizeConfig(config)
	return &Executor{
		planner: NewPlanner(domain, cfg),
		handler: handler,
		config:  cfg,
	}
}

// Run 从规划请求开始执行，进度按提交位置持久化。
func (e *Executor) Run(ctx context.Context, req PlanRequest) (*ExecutionReport, error) {
	if req.ID == "" {
		return nil, invalidf("plan request must have an ID for persistence")
	}
	var st *store
	if e.config.PersistDir != "" {
		st = newStore(e.config.PersistDir)
		if existing, err := st.load(req.ID); err == nil && existing != nil {
			return e.resumeFrom(ctx, st, existing)
		} else if !errors.Is(err, errNoJournal) {
			return nil, err
		}
	}

	if err := e.planner.domain.Validate(); err != nil {
		return nil, err
	}
	if err := validateRequest(e.planner.domain, req); err != nil {
		return nil, err
	}

	state := NewState(req.InitialState...)
	net := rootNetwork(req.RootTasks)
	j := &journal{
		PlanID:     req.ID,
		Request:    req,
		StateAtoms: state.AtomKeys(),
		Network:    networkToStored(net),
		Status:     journalWaveCommitted,
	}
	return e.runLoop(ctx, st, j, state, net)
}

// Resume 从持久化目录中指定计划（planID 为空时取最近一次）的已提交位置恢复执行。
func (e *Executor) Resume(ctx context.Context, planID string) (*ExecutionReport, error) {
	if e.config.PersistDir == "" {
		return nil, invalidf("persistence is disabled: PersistDir is empty")
	}
	st := newStore(e.config.PersistDir)
	var (
		j   *journal
		err error
	)
	if planID == "" {
		j, err = st.latest()
	} else {
		j, err = st.load(planID)
	}
	if err != nil {
		return nil, err
	}
	return e.resumeFrom(ctx, st, j)
}

func (e *Executor) resumeFrom(ctx context.Context, st *store, j *journal) (*ExecutionReport, error) {
	state := newStateFromKeys(j.StateAtoms)
	net := storedToNetwork(j.Network)

	// 崩溃可能发生在某个波执行途中：波效果尚未提交，丢弃在途波并从最近提交点重跑。
	if j.Status == journalWaveStarted {
		j.Status = journalWaveCommitted
		j.InFlight = nil
		// HeadWaves 保留：它记录该高层组尚未提交的剩余波，恢复时从这里重放。
	}
	// 封禁集合属于进程内重规划控制状态：重启后从最近提交点干净恢复。
	j.Banned = nil

	if err := e.planner.domain.Validate(); err != nil {
		return nil, err
	}
	return e.runLoop(ctx, st, j, state, net)
}

type actionOutcome struct {
	action *GroundAction
	result *ActionResult
	err    error
}

// runLoop 是执行器主循环：展开队首高层任务组 -> 逐波并发驱动 -> 提交效果 -> 持久化。
// 失败/偏差时保留高层任务组结构，从当前状态重新分解，已执行动作不会重复执行。
// runLoop 是执行器主循环。
//
// 恢复模型：
//   - 每次展开队首高层任务组，得到一组有序并行波；
//   - 队首波执行失败时（stop），尚无已提交效果，可从当前状态重新分解该高层组，
//     从而回退到其它分解方式（封禁失败动作以避开同一路径）；
//   - 后续波失败或进程重启时，高层前缀效果已提交，直接重放剩余已展开波，
//     已提交波绝不重复执行；
//   - continue 策略下失败动作与成功兄弟分支解耦：成功分支提交，失败动作作为
//     原始任务在当前状态上单独重试；
//   - 观察效果偏差时提交观察状态，剩余波在真实状态上逐动作复验后重放。
func (e *Executor) runLoop(ctx context.Context, st *store, j *journal, state *State, net *taskNetwork) (*ExecutionReport, error) {
	banned := sliceToBanned(j.Banned)
	report := &ExecutionReport{PlanID: j.PlanID, Status: "running"}

	persist := func() error {
		if st == nil {
			return nil
		}
		j.StateAtoms = state.AtomKeys()
		j.Network = networkToStored(net)
		j.Banned = bannedToSlice(banned)
		return st.save(j)
	}

groupLoop:
	for len(net.groups) > 0 {
		// 1) 取得当前高层组的待执行波。
		var waves []*Wave
		if len(j.HeadWaves) > 0 {
			for _, sw := range j.HeadWaves {
				waves = append(waves, storedToWave(sw))
			}
		} else {
			w, _, err := e.planner.expandGroup(state, net.groups[0], banned)
			if err != nil {
				// 封禁后无可行分解（无其它方法可回退）：视为瞬时故障，
				// 解除封禁重试原动作一次；仍受 MaxReplans 计数保护。
				w, _, err = e.planner.expandGroup(state, net.groups[0], nil)
				if err != nil {
					return nil, classifyExpansion(err)
				}
				banned = map[string]struct{}{}
				j.Banned = nil
			}
			waves = w
			j.HeadWaves = wavesToStored(waves)
			j.HeadIndex = 0
		}

		firstWave := true

		for len(waves) > 0 {
			wave := waves[0]
			j.HeadIndex = len(j.HeadWaves) - len(waves)
			j.InFlight = waveToStoredActions(wave, "in_flight")
			j.Status = journalWaveStarted
			if err := persist(); err != nil {
				return nil, err
			}

			outcomes := e.runWave(ctx, wave)

			// 调用方主动取消：立即终止，不触发重规划（journal 已记录可在后续恢复）。
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			var failed []*GroundAction
			var okActions []*GroundAction
			actuals := make(map[string]*ActionResult)
			for _, oc := range outcomes {
				if oc.err != nil {
					failed = append(failed, oc.action)
					continue
				}
				okActions = append(okActions, oc.action)
				actuals[oc.action.Key()] = oc.result
			}
			sort.Slice(failed, func(i, k int) bool { return failed[i].Key() < failed[k].Key() })
			sort.Slice(okActions, func(i, k int) bool { return okActions[i].Key() < okActions[k].Key() })

			if len(failed) > 0 {
				if j.Replans >= e.config.MaxReplans {
					return nil, &PlanError{Kind: FailureExecution, Message: "execution failure and replan budget exhausted"}
				}
				j.Replans++
				report.Replans = j.Replans

				if e.config.FailurePolicy == PolicyContinue {
					// 成功分支提交；失败动作作为原始任务在当前状态上展开为重试波，
					// 拼在剩余波之前，继续执行。
					for _, g := range okActions {
						commitOne(g, actuals[g.Key()], state, report)
					}
					retryGroup := make([]groundTask, 0, len(failed))
					for _, g := range failed {
						retryGroup = append(retryGroup, groundTask{
							name: g.Name, args: append([]string(nil), g.Args...), path: g.ID + "/retry",
						})
						banned[g.Key()] = struct{}{}
					}
					retryWaves, _, err := e.planner.expandGroup(state, retryGroup, nil)
					if err != nil {
						return nil, classifyExpansion(err)
					}
					waves = append(retryWaves, waves[1:]...)
					j.HeadWaves = wavesToStored(waves)
					j.HeadIndex = 0
					j.InFlight = nil
					j.Status = journalWaveCommitted
					if err := persist(); err != nil {
						return nil, err
					}
					firstWave = false
					continue
				}

				// stop 策略：本波效果整体不提交。
				// 封禁失败动作以促使复合任务改选其它方法；若无其它方法，
				// 外层展开会在封禁无解时解除封禁重试（瞬时故障恢复）。
				banned[failed[0].Key()] = struct{}{}
				j.InFlight = nil
				j.Status = journalWaveCommitted
				if firstWave {
					// 高层组第一个波失败：丢弃整个展开，回到外层重新分解同一高层组。
					j.HeadWaves = nil
					j.HeadIndex = 0
					if err := persist(); err != nil {
						return nil, err
					}
					continue groupLoop
				}
				// 后续波失败：重放当前波及其后剩余已展开波（动作将在真实状态上复验）。
				if err := persist(); err != nil {
					return nil, err
				}
				continue
			}

			// 无失败：提交效果并检测偏差。
			discrepancy := false
			for _, g := range okActions {
				r := actuals[g.Key()]
				if r != nil && !r.AsExpected {
					state.Apply(r.ObservedAdd, r.ObservedDel)
					discrepancy = true
				} else {
					state.Apply(g.Add, g.Del)
				}
				report.Executed = append(report.Executed, ActionRecord{
					ID: g.ID, Name: g.Name, Args: append([]string(nil), g.Args...), Status: "ok",
				})
			}
			waves = waves[1:]
			if discrepancy {
				// 偏差：已提交波的观察效果生效，放弃该高层组剩余展开，
				// 在当前真实状态上重新分解原始高层组（可能选择不同方法）。
				if j.Replans >= e.config.MaxReplans {
					return nil, &PlanError{Kind: FailureExecution, Message: "discrepancy replan budget exhausted"}
				}
				j.Replans++
				report.Replans = j.Replans
				waves = nil
				j.HeadWaves = nil
				j.HeadIndex = 0
				j.InFlight = nil
				j.Status = journalWaveCommitted
				if err := persist(); err != nil {
					return nil, err
				}
				continue groupLoop
			}
			j.HeadWaves = wavesToStored(waves)
			j.HeadIndex = 0
			j.InFlight = nil
			j.Status = journalWaveCommitted
			if err := persist(); err != nil {
				return nil, err
			}
			firstWave = false
		}

		net.groups = net.groups[1:]
		j.HeadWaves = nil
		j.HeadIndex = 0
		j.InFlight = nil
		if err := persist(); err != nil {
			return nil, err
		}
	}

	if !state.satisfies(j.Request.Goals) {
		return nil, &PlanError{Kind: FailureUnreachable, Message: "goals not satisfied after execution"}
	}
	j.Status = journalComplete
	if st != nil {
		if err := st.save(j); err != nil {
			return nil, err
		}
	}
	report.Status = "complete"
	report.FinalState = state
	return report, nil
}

func commitOne(g *GroundAction, r *ActionResult, st *State, report *ExecutionReport) {
	if r != nil && !r.AsExpected {
		st.Apply(r.ObservedAdd, r.ObservedDel)
	} else {
		st.Apply(g.Add, g.Del)
	}
	report.Executed = append(report.Executed, ActionRecord{
		ID: g.ID, Name: g.Name, Args: append([]string(nil), g.Args...), Status: "ok",
	})
}

func (e *Executor) runWave(parent context.Context, wave *Wave) []actionOutcome {
	results := make([]actionOutcome, len(wave.Actions))
	for i := range results {
		results[i] = actionOutcome{action: wave.Actions[i]}
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sem := make(chan struct{}, e.config.MaxParallel)
	var wg sync.WaitGroup

	for i := range wave.Actions {
		i := i
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			if results[i].err == nil {
				results[i] = actionOutcome{action: wave.Actions[i], err: ctx.Err()}
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := e.handler(ctx, wave.Actions[i])
			results[i] = actionOutcome{action: wave.Actions[i], result: res, err: err}
			if err != nil && e.config.FailurePolicy == PolicyStopOnError {
				cancel()
			}
		}()
	}
	wg.Wait()

	sort.SliceStable(results, func(i, k int) bool {
		return results[i].action.Key() < results[k].action.Key()
	})
	return results
}

func classifyExpansion(err error) error {
	if pe, ok := err.(*PlanError); ok {
		if pe.Kind == FailureBudgetExceeded {
			return &PlanError{Kind: FailureExecution, Message: "replan budget exceeded from current state: " + pe.Message}
		}
		return &PlanError{Kind: FailureExecution, Message: "replanning from current state failed: " + pe.Message}
	}
	return err
}

func sameState(a, b *State) bool {
	ka, kb := a.AtomKeys(), b.AtomKeys()
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}
