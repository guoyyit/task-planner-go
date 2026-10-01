// Package taskplan implements hierarchical task network (HTN) planning
// and monitored plan execution entirely in-process. A caller declares a
// Domain (primitive actions with preconditions/effects, and compound
// tasks with one or more decomposition methods), then asks a Planner for
// a Plan achieving a list of goal tasks, and finally drives the plan with
// an Executor that monitors deviations and replans when needed.
package taskplan

import "sort"

// State is the set of facts currently true in the world. Facts are
// free-form strings such as "at robot1 hall" or "door-open".
//
// The zero value is an empty state and ready to use.
type State struct {
	facts map[string]struct{}
}

// NewState returns a State containing exactly the given facts.
func NewState(facts ...string) State {
	s := State{facts: make(map[string]struct{}, len(facts))}
	for _, f := range facts {
		s.facts[f] = struct{}{}
	}
	return s
}

// Has reports whether fact f holds in s.
func (s State) Has(f string) bool {
	_, ok := s.facts[f]
	return ok
}

// Clone returns a deep copy of s.
func (s State) Clone() State {
	out := State{facts: make(map[string]struct{}, len(s.facts))}
	for f := range s.facts {
		out.facts[f] = struct{}{}
	}
	return out
}

// Apply mutates s by deleting e.Del and then adding e.Add.
func (s State) Apply(e Effects) {
	if s.facts == nil {
		s.facts = make(map[string]struct{}, len(e.Add))
	}
	for _, f := range e.Del {
		delete(s.facts, f)
	}
	for _, f := range e.Add {
		s.facts[f] = struct{}{}
	}
}

// Sorted returns the facts of s in deterministic (lexicographic) order.
func (s State) Sorted() []string {
	out := make([]string, 0, len(s.facts))
	for f := range s.facts {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Equal reports whether s and o contain exactly the same facts.
func (s State) Equal(o State) bool {
	if len(s.facts) != len(o.facts) {
		return false
	}
	for f := range s.facts {
		if !o.Has(f) {
			return false
		}
	}
	return true
}

// Conditions are the preconditions of an action or a decomposition
// method: every fact in Pos must hold and every fact in Neg must not.
type Conditions struct {
	Pos []string `json:"pos,omitempty"`
	Neg []string `json:"neg,omitempty"`
}

// SatisfiedBy reports whether all conditions hold in state s.
func (c Conditions) SatisfiedBy(s State) bool {
	for _, f := range c.Pos {
		if !s.Has(f) {
			return false
		}
	}
	for _, f := range c.Neg {
		if s.Has(f) {
			return false
		}
	}
	return true
}

// Effects describe how an action changes the world: facts in Del are
// removed, then facts in Add are inserted.
type Effects struct {
	Add []string `json:"add,omitempty"`
	Del []string `json:"del,omitempty"`
}

// Action is a primitive, directly executable operator of the domain.
type Action struct {
	Name string     `json:"name"`
	Pre  Conditions `json:"pre,omitempty"`
	Eff  Effects    `json:"eff,omitempty"`
}

// TaskKind distinguishes primitive tasks (actions) from compound tasks.
type TaskKind int

const (
	// PrimitiveTask names an Action declared in Domain.Actions.
	PrimitiveTask TaskKind = iota
	// CompoundTask names a task decomposed by Domain.Methods.
	CompoundTask
)

// TaskRef is a reference to a task to accomplish. ID is assigned by the
// planner to distinguish occurrences of the same task inside one plan;
// callers building goal lists should leave it zero.
type TaskRef struct {
	Kind TaskKind `json:"kind"`
	Name string   `json:"name"`
	ID   uint64   `json:"id,omitempty"`
}

// Primitive returns a TaskRef referencing the named action.
func Primitive(name string) TaskRef {
	return TaskRef{Kind: PrimitiveTask, Name: name}
}

// Compound returns a TaskRef referencing the named compound task.
func Compound(name string) TaskRef {
	return TaskRef{Kind: CompoundTask, Name: name}
}

// Method declares one way to decompose a compound task. A domain may
// declare several methods for the same task; the planner tries them in
// declaration order and falls back to the next one when a method's
// preconditions do not hold or its subtree fails to plan.
type Method struct {
	Name     string     `json:"name"`
	Pre      Conditions `json:"pre,omitempty"`
	Subtasks []TaskRef  `json:"subtasks,omitempty"`
}

// Domain describes everything the planner may use: primitive actions and
// decomposition methods for compound tasks. Methods maps a compound task
// name to its candidate methods in declaration order.
type Domain struct {
	Actions map[string]*Action   `json:"actions,omitempty"`
	Methods map[string][]*Method `json:"methods,omitempty"`
}

// PlannerConfig tunes the planner. The zero value uses the defaults.
type PlannerConfig struct {
	// MaxExpansions bounds the number of task expansions (agenda pops)
	// the planner may perform. Zero means DefaultMaxExpansions.
	// Exceeding the budget fails with FailureBudgetExceeded.
	MaxExpansions int `json:"maxExpansions,omitempty"`
}

// DefaultMaxExpansions is the planning budget used when
// PlannerConfig.MaxExpansions is zero.
const DefaultMaxExpansions = 10000

func (c PlannerConfig) maxExpansions() int {
	if c.MaxExpansions <= 0 {
		return DefaultMaxExpansions
	}
	return c.MaxExpansions
}
