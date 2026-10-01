package taskplan

// item is an agenda entry: a task occurrence plus the index of the
// top-level goal it descends from.
type item struct {
	task TaskRef
	goal int
}

// choice is a backtracking point created when a compound task is
// expanded: it remembers the untried methods and the search state to
// restore when a later failure forces trying the next method.
type choice struct {
	methods  []*Method
	next     int
	agenda   []item
	state    State
	stepsLen int
}

// Planner performs totally-ordered HTN planning with backtracking over
// alternative decomposition methods. It is deterministic: identical
// inputs always produce an identical Plan.
type Planner struct {
	cfg PlannerConfig
}

// NewPlanner returns a Planner using the given configuration.
func NewPlanner(cfg PlannerConfig) *Planner {
	return &Planner{cfg: cfg}
}

// Plan decomposes the goal tasks into a sequence of primitive steps
// whose preconditions hold at their execution position, starting from
// initial. It validates the domain first and fails with:
//
//   - FailureInvalidDomain   – malformed domain or unknown goal tasks
//   - FailureGoalUnreachable – no method combination achieves the goals
//   - FailureBudgetExceeded  – expansion budget exhausted
func (p *Planner) Plan(domain *Domain, initial State, goals []TaskRef) (*Plan, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	for _, g := range goals {
		if err := domain.checkTaskRef("goal", g); err != nil {
			return nil, err
		}
	}

	maxExp := p.cfg.maxExpansions()
	expansions := 0
	overBudget := false
	spend := func() bool {
		expansions++
		if expansions > maxExp {
			overBudget = true
			return false
		}
		return true
	}

	var nextID uint64 = 1
	instantiate := func(subtasks []TaskRef, goal int) []item {
		out := make([]item, len(subtasks))
		for i, st := range subtasks {
			st.ID = nextID
			nextID++
			out[i] = item{task: st, goal: goal}
		}
		return out
	}

	agenda := instantiate(goals, 0)
	for i := range agenda {
		agenda[i].goal = i
	}
	state := initial.Clone()
	var steps []Step
	var choices []*choice

	// tryMethods expands the compound task at the head of cp.agenda with
	// the next applicable untried method. It assumes state and agenda
	// have already been restored to cp.
	tryMethods := func(cp *choice) bool {
		for cp.next < len(cp.methods) {
			m := cp.methods[cp.next]
			cp.next++
			if m.Pre.SatisfiedBy(state) {
				subs := instantiate(m.Subtasks, agenda[0].goal)
				agenda = append(subs, agenda[1:]...)
				return true
			}
		}
		return false
	}

	backtrack := func() bool {
		for len(choices) > 0 {
			if !spend() {
				return false
			}
			cp := choices[len(choices)-1]
			agenda = cp.agenda
			state = cp.state.Clone()
			steps = steps[:cp.stepsLen]
			if tryMethods(cp) {
				return true
			}
			choices = choices[:len(choices)-1]
		}
		return false
	}

	for len(agenda) > 0 {
		if !spend() {
			return nil, budgetf("expansion budget %d exhausted", maxExp)
		}
		it := agenda[0]
		if it.task.Kind == PrimitiveTask {
			a := domain.Actions[it.task.Name]
			if a.Pre.SatisfiedBy(state) {
				steps = append(steps, Step{
					Index:     len(steps),
					Action:    a.Name,
					GoalIndex: it.goal,
					TaskID:    it.task.ID,
					Agenda:    snapshotTasks(agenda),
				})
				state.Apply(a.Eff)
				agenda = agenda[1:]
				continue
			}
			if !backtrack() {
				if overBudget {
					return nil, budgetf("expansion budget %d exhausted", maxExp)
				}
				return nil, unreachablef("preconditions of action %q cannot be satisfied", it.task.Name)
			}
			continue
		}
		cp := &choice{
			methods:  domain.Methods[it.task.Name],
			agenda:   append([]item(nil), agenda...),
			state:    state.Clone(),
			stepsLen: len(steps),
		}
		choices = append(choices, cp)
		if !tryMethods(cp) {
			choices = choices[:len(choices)-1]
			if !backtrack() {
				if overBudget {
					return nil, budgetf("expansion budget %d exhausted", maxExp)
				}
				return nil, unreachablef("no applicable method for compound task %q", it.task.Name)
			}
		}
	}

	plan := &Plan{Steps: steps}
	plan.Layers = computeLayers(steps, domain)
	return plan, nil
}

// snapshotTasks copies the task references of the agenda into a fresh
// slice attached to an emitted step.
func snapshotTasks(agenda []item) []TaskRef {
	out := make([]TaskRef, len(agenda))
	for i, it := range agenda {
		out[i] = it.task
	}
	return out
}
