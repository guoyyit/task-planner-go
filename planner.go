package taskplanner

import "context"

import (
	"fmt"
	"sort"
	"strings"
)

// Config 是规划与执行配置，零值由 NewPlanner/NewExecutor 填充默认值。
type Config struct {
	// Budget 规划过程中复合任务分解次数上限，0 表示使用默认值。
	Budget int
	// MaxParallel 单个并行集合内允许的最大并发动作数，0 表示使用默认值。
	MaxParallel int
	// MaxReplans 执行过程中最多重新规划次数；0 使用默认值，-1 表示禁止任何重规划。
	MaxReplans int
	// FailurePolicy 并行分支失败策略："stop"（停止其余分支）或 "continue"（继续）。
	FailurePolicy string
	// PersistDir 进度持久化目录，空表示不持久化。
	PersistDir string
}

// PlanRequest 是一次规划请求：从初始状态执行根任务序列，并在终态满足全部目标。
type PlanRequest struct {
	ID           string
	InitialState []Literal
	RootTasks    []TaskExpr
	Goals        []Literal
}

// Wave 是计划中一个可并行执行的动作集合。
type Wave struct {
	ID      string
	Actions []*GroundAction
}

// Plan 是层次化分解后得到的有序并行集合序列。
type Plan struct {
	Waves []*Wave
}

// Planner 是层次化任务规划器。
type Planner struct {
	domain *Domain
	config Config
}

func NewPlanner(domain *Domain, config Config) *Planner {
	return &Planner{domain: domain, config: normalizeConfig(config)}
}

// Plan 执行完整规划，返回计划、预期终态与可区分的失败结果。
func (p *Planner) Plan(ctx context.Context, req PlanRequest) (*Plan, *State, error) {
	if err := p.domain.Validate(); err != nil {
		return nil, nil, err
	}
	if err := validateRequest(p.domain, req); err != nil {
		return nil, nil, err
	}

	st := NewState(req.InitialState...)
	net := rootNetwork(req.RootTasks)
	s := &searcher{
		domain:      p.domain,
		budgetLeft:  p.config.Budget,
		maxParallel: p.config.MaxParallel,
		banned:      map[string]struct{}{},
	}

	var planWaves []*Wave
	err := s.solveSeq(st, net.groups, nil, func(waves []*Wave, final *State) error {
		if !final.satisfies(req.Goals) {
			return errBacktrack
		}
		planWaves = waves
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// 规划后防御性复验：每个动作在其执行位置必须满足全部前置条件。
	materialized := materialize(planWaves)
	check := NewState(req.InitialState...)
	for _, w := range materialized {
		for _, g := range w.Actions {
			if !check.satisfies(g.Pre) {
				return nil, nil, &PlanError{Kind: FailureUnreachable, Message: "internal: precondition not satisfied at execution position: " + g.Key()}
			}
		}
		for _, g := range w.Actions {
			check.Apply(g.Add, g.Del)
		}
	}
	if !check.satisfies(req.Goals) {
		return nil, nil, &PlanError{Kind: FailureUnreachable, Message: "internal: goals not satisfied by plan"}
	}
	return &Plan{Waves: materialized}, check, nil
}

// materialize 为波分配确定性序号 ID，并按动作实例键稳定排序。
func materialize(waves []*Wave) []*Wave {
	out := make([]*Wave, len(waves))
	for i, w := range waves {
		acts := append([]*GroundAction(nil), w.Actions...)
		sort.Slice(acts, func(a, b int) bool {
			if acts[a].ID != acts[b].ID {
				return acts[a].ID < acts[b].ID
			}
			return acts[a].Key() < acts[b].Key()
		})
		out[i] = &Wave{ID: fmt.Sprintf("wave-%d", i), Actions: acts}
	}
	return out
}

// groundTask 是展开网络中一个已基例化的任务节点。
type groundTask struct {
	name string
	args []string
	path string
}

// taskNetwork 是尚待展开/执行的任务网络：有序的任务组，组内可并行。
type taskNetwork struct {
	groups [][]groundTask
}

// ---- 内部实现 ----

var (
	errBacktrack = &PlanError{Kind: FailureUnreachable, Message: "backtrack"}
	errBudget    = &PlanError{Kind: FailureBudgetExceeded, Message: "planning budget exceeded"}
	errConflict  = &PlanError{Kind: FailureUnreachable, Message: "cross-child resource conflict"}
)

const (
	defaultBudget      = 1000
	defaultMaxParallel = 8
	defaultMaxReplans  = 4
)

func normalizeConfig(c Config) Config {
	if c.Budget <= 0 {
		c.Budget = defaultBudget
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = defaultMaxParallel
	}
	if c.MaxReplans < 0 {
		c.MaxReplans = 0
	}
	if c.MaxReplans == 0 {
		c.MaxReplans = defaultMaxReplans
	}
	if c.FailurePolicy != PolicyContinue {
		c.FailurePolicy = PolicyStopOnError
	}
	return c
}

func validateGroundLits(owner string, lits []Literal) error {
	for _, lit := range lits {
		if lit.Predicate == "" {
			return invalidf("%s: literal has empty predicate", owner)
		}
		for _, a := range lit.Args {
			if strings.HasPrefix(a, "?") {
				return invalidf("%s: literal %s contains unbound variable %s", owner, atomKey(lit), a)
			}
		}
	}
	return nil
}

// validateRequest 校验规划请求中的根任务与目标均为合法基例引用。
func validateRequest(d *Domain, req PlanRequest) error {
	if err := validateGroundLits("initial state", req.InitialState); err != nil {
		return err
	}
	if err := validateGroundLits("goals", req.Goals); err != nil {
		return err
	}
	for i, t := range req.RootTasks {
		owner := fmt.Sprintf("root task %d", i)
		if t.Name == "" {
			return invalidf("%s: empty task name", owner)
		}
		ac, isAction := d.Actions[t.Name]
		cp, isCompound := d.Compounds[t.Name]
		switch {
		case isAction:
			if len(t.Args) != len(ac.Params) {
				return invalidf("%s: action %s expects %d args, got %d", owner, t.Name, len(ac.Params), len(t.Args))
			}
		case isCompound:
			if len(t.Args) != len(cp.Params) {
				return invalidf("%s: compound task %s expects %d args, got %d", owner, t.Name, len(cp.Params), len(t.Args))
			}
		default:
			return invalidf("%s: unknown task %q", owner, t.Name)
		}
		for _, a := range t.Args {
			if strings.HasPrefix(a, "?") {
				return invalidf("%s: argument %s must be ground", owner, a)
			}
		}
	}
	return nil
}

func rootNetwork(tasks []TaskExpr) *taskNetwork {
	net := &taskNetwork{groups: make([][]groundTask, len(tasks))}
	for i, t := range tasks {
		net.groups[i] = []groundTask{{name: t.Name, args: append([]string(nil), t.Args...), path: fmt.Sprintf("root#%d", i)}}
	}
	return net
}

func bindTask(t TaskExpr, binding map[string]string, path string) groundTask {
	args := make([]string, len(t.Args))
	for i, a := range t.Args {
		if strings.HasPrefix(a, "?") {
			if v, ok := binding[a]; ok {
				args[i] = v
				continue
			}
		}
		args[i] = a
	}
	return groundTask{name: t.Name, args: args, path: path}
}

type childPlan struct {
	waves []*Wave
	final *State
}

type searchFn func(waves []*Wave, st *State) error

type searcher struct {
	domain      *Domain
	budgetLeft  int
	maxParallel int
	banned      map[string]struct{}
}

// useBudget 登记一次复合任务分解尝试，预算耗尽则全局终止搜索。
func (s *searcher) useBudget() error {
	s.budgetLeft--
	if s.budgetLeft < 0 {
		return errBudget
	}
	return nil
}

// solveSeq 顺序规划一组有序任务组：前一组全部完成后才进入下一组。
func (s *searcher) solveSeq(st *State, groups [][]groundTask, waves []*Wave, k searchFn) error {
	if len(groups) == 0 {
		return k(waves, st)
	}
	return s.solveGroup(st, groups[0], func(w []*Wave, st1 *State) error {
		return s.solveSeq(st1, groups[1:], append(waves, w...), k)
	})
}

// solveGroup 规划一个并行任务组：枚举每个成员任务的展开方式，
// 无冲突时按层次合并为并行波，存在跨成员冲突时退化为顺序执行。
func (s *searcher) solveGroup(st *State, tasks []groundTask, k searchFn) error {
	if len(tasks) == 0 {
		return k(nil, st)
	}
	if len(tasks) == 1 {
		return s.solveTask(st, tasks[0], k)
	}

	// 先基于共同起始状态枚举各成员计划；若跨成员无冲突则并行合并。
	children := make([]childPlan, 0, len(tasks))
	var chooseParallel func(int) error
	chooseParallel = func(i int) error {
		if i == len(tasks) {
			if crossChildConflict(children) {
				return errConflict
			}
			return s.composeParallel(st, children, k)
		}
		err := s.solveTask(st, tasks[i], func(w []*Wave, st1 *State) error {
			children = append(children, childPlan{waves: w, final: st1})
			err := chooseParallel(i + 1)
			children = children[:len(children)-1]
			return err
		})
		// 某成员在共同起始状态下无解：可能依赖其它成员先建立前置条件，
		// 转入顺序模式，让前置条件沿真实执行链路建立。
		if err == errBacktrack {
			return errConflict
		}
		return err
	}

	// 顺序回退：每个成员在前一个成员终态上展开（方法前置条件基于真实链路评估）。
	var solveSeqChildren func(int, *State, []*Wave) error
	solveSeqChildren = func(i int, cur *State, acc []*Wave) error {
		if i == len(tasks) {
			return k(acc, cur)
		}
		return s.solveTask(cur, tasks[i], func(w []*Wave, st1 *State) error {
			return solveSeqChildren(i+1, st1, append(acc, w...))
		})
	}

	err := chooseParallel(0)
	if err == errConflict {
		return solveSeqChildren(0, st, nil)
	}
	return err
}

// solveTask 规划单个任务：基元动作直接发射为单动作波；
// 复合任务按声明顺序尝试各方法，失败则回溯下一种分解方式。
func (s *searcher) solveTask(st *State, t groundTask, k searchFn) error {
	if ac, ok := s.domain.Actions[t.name]; ok {
		g := groundAction(ac, t.args)
		g.ID = t.path
		if _, bad := s.banned[g.Key()]; bad {
			return errBacktrack
		}
		if !st.satisfies(g.Pre) {
			return errBacktrack
		}
		st1 := st.Clone()
		st1.Apply(g.Add, g.Del)
		return k([]*Wave{{ID: t.path, Actions: []*GroundAction{g}}}, st1)
	}
	cp := s.domain.Compounds[t.name]
	binding := methodBinding(cp.Params, t.args)
	for mi, m := range cp.Methods {
		if err := s.useBudget(); err != nil {
			return err
		}
		pre := bindAll(m.Pre, binding)
		if !st.satisfies(pre) {
			continue
		}
		groups := make([][]groundTask, len(m.Groups))
		for gi, group := range m.Groups {
			groups[gi] = make([]groundTask, len(group))
			for ti, sub := range group {
				groups[gi][ti] = bindTask(sub, binding, fmt.Sprintf("%s/m%d/g%d#%d", t.path, mi, gi, ti))
			}
		}
		err := s.solveSeq(st, groups, nil, k)
		if err == nil {
			return nil
		}
		if err != errBacktrack {
			return err
		}
	}
	return errBacktrack
}

// actionsConflict 判断两个动作是否不能放入同一并行波：
// 任一方写集合触及另一方的读或写集合即冲突。
func actionsConflict(a, b *GroundAction) bool {
	aw := a.Writes()
	bw := b.Writes()
	ar := a.Reads()
	br := b.Reads()
	return intersects(aw, bw) || intersects(aw, br) || intersects(ar, bw)
}

func intersects(a, b []string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// crossChildConflict 检查不同子任务计划之间是否存在资源冲突。
func crossChildConflict(children []childPlan) bool {
	for i := 0; i < len(children); i++ {
		for j := i + 1; j < len(children); j++ {
			for _, w1 := range children[i].waves {
				for _, w2 := range children[j].waves {
					for _, a := range w1.Actions {
						for _, b := range w2.Actions {
							if actionsConflict(a, b) {
								return true
							}
						}
					}
				}
			}
		}
	}
	return false
}

// composeParallel 在无跨子任务冲突的前提下，按层次贪心合并各子任务的波，
// 同一波内动作两两无依赖/无冲突，并受 MaxParallel 限制。
func (s *searcher) composeParallel(st *State, children []childPlan, k searchFn) error {
	cursor := make([]int, len(children))
	var waves []*Wave
	cur := st.Clone()

	for {
		var level []*GroundAction
		advanced := false
		for ci := range children {
			if cursor[ci] >= len(children[ci].waves) {
				continue
			}
			candidates := append([]*GroundAction(nil), children[ci].waves[cursor[ci]].Actions...)
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].Key() < candidates[j].Key() })
			chosen := make([]*GroundAction, 0, len(candidates))
			blocked := false
			for _, g := range candidates {
				if len(level)+len(chosen) >= s.maxParallel {
					blocked = true
					break
				}
				conflict := false
				for _, in := range level {
					for _, in2 := range chosen {
						if actionsConflict(g, in2) {
							conflict = true
							break
						}
					}
					if conflict {
						break
					}
					if actionsConflict(g, in) {
						conflict = true
						break
					}
				}
				if conflict {
					blocked = true
					break
				}
				chosen = append(chosen, g)
			}
			if blocked {
				continue
			}
			level = append(level, chosen...)
			cursor[ci]++
			advanced = true
		}
		if len(level) == 0 {
			if !advanced && allDone(children, cursor) {
				break
			}
			// 无任何子任务推进但尚有剩余：通常不应发生，返回不可达而非死循环。
			return errBacktrack
		}
		sort.Slice(level, func(i, j int) bool { return level[i].Key() < level[j].Key() })
		for _, g := range level {
			if !cur.satisfies(g.Pre) {
				return errBacktrack
			}
		}
		for _, g := range level {
			cur.Apply(g.Add, g.Del)
		}
		waves = append(waves, &Wave{Actions: level})
	}
	return k(waves, cur)
}

func allDone(children []childPlan, cursor []int) bool {
	for i := range children {
		if cursor[i] < len(children[i].waves) {
			return false
		}
	}
	return true
}

// expandGroup 展开任务网络队首的一个高层任务组，返回该组在当前状态下的
// 全部并行波与预期终态。失败（含 banned 动作）时返回 errBacktrack，
// 调用方可保留高层组结构再次尝试其它分解方式。
func (p *Planner) expandGroup(st *State, group []groundTask, banned map[string]struct{}) ([]*Wave, *State, error) {
	s := &searcher{
		domain:      p.domain,
		budgetLeft:  p.config.Budget,
		maxParallel: p.config.MaxParallel,
		banned:      banned,
	}
	var picked []*Wave
	var final *State
	err := s.solveGroup(st, group, func(waves []*Wave, st1 *State) error {
		picked = waves
		final = st1
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return materialize(picked), final, nil
}
