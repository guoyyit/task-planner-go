package taskplan

import (
	"reflect"
	"testing"
)

// robotDomain models a robot fetching a key: it can walk to the room
// and pick the key there, and the compound task "fetch-key" offers two
// decompositions, the optimistic one first.
func robotDomain() *Domain {
	return &Domain{
		Actions: map[string]*Action{
			"move-hall-room": {
				Name: "move-hall-room",
				Pre:  Conditions{Pos: []string{"at-hall"}},
				Eff:  Effects{Add: []string{"at-room"}, Del: []string{"at-hall"}},
			},
			"open-door": {
				Name: "open-door",
				Pre:  Conditions{Pos: []string{"at-hall"}},
				Eff:  Effects{Add: []string{"door-open"}},
			},
			"pick-key": {
				Name: "pick-key",
				Pre:  Conditions{Pos: []string{"at-room", "key-in-room"}},
				Eff:  Effects{Add: []string{"have-key"}, Del: []string{"key-in-room"}},
			},
		},
		Methods: map[string][]*Method{
			"fetch-key": {
				{Name: "optimistic", Subtasks: []TaskRef{Primitive("pick-key")}},
				{Name: "walk-and-pick", Subtasks: []TaskRef{Primitive("move-hall-room"), Primitive("pick-key")}},
			},
		},
	}
}

func TestPlanBacktracksToAlternativeMethod(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	plan, err := p.Plan(robotDomain(), NewState("at-hall", "key-in-room"), []TaskRef{Compound("fetch-key")})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	want := []string{"move-hall-room", "pick-key"}
	if got := plan.Actions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
}

func TestPlanUsesFirstMethodWhenApplicable(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	plan, err := p.Plan(robotDomain(), NewState("at-room", "key-in-room"), []TaskRef{Compound("fetch-key")})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if got := plan.Actions(); !reflect.DeepEqual(got, []string{"pick-key"}) {
		t.Fatalf("actions = %v, want [pick-key]", got)
	}
}

func TestPlanMethodPreconditionsSelectDecomposition(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{
			"ring-bell": {Name: "ring-bell", Eff: Effects{Add: []string{"door-open"}}},
			"knock":     {Name: "knock", Eff: Effects{Add: []string{"door-open"}}},
		},
		Methods: map[string][]*Method{
			"get-in": {
				{Name: "polite", Pre: Conditions{Pos: []string{"bell-works"}}, Subtasks: []TaskRef{Primitive("ring-bell")}},
				{Name: "loud", Subtasks: []TaskRef{Primitive("knock")}},
			},
		},
	}
	p := NewPlanner(PlannerConfig{})
	plan, err := p.Plan(d, NewState(), []TaskRef{Compound("get-in")})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if got := plan.Actions(); !reflect.DeepEqual(got, []string{"knock"}) {
		t.Fatalf("actions = %v, want [knock]", got)
	}
}

func TestPlanUnreachableGoal(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	_, err := p.Plan(robotDomain(), NewState("at-hall"), []TaskRef{Primitive("pick-key")})
	if !IsFailure(err, FailureGoalUnreachable) {
		t.Fatalf("err = %v, want FailureGoalUnreachable", err)
	}
}

func TestPlanRejectsRecursiveCycle(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{
			"noop": {Name: "noop"},
		},
		Methods: map[string][]*Method{
			"a": {{Name: "a-to-b", Subtasks: []TaskRef{Compound("b")}}},
			"b": {{Name: "b-to-a", Subtasks: []TaskRef{Compound("a")}}},
		},
	}
	p := NewPlanner(PlannerConfig{})
	_, err := p.Plan(d, NewState(), []TaskRef{Compound("a")})
	if !IsFailure(err, FailureInvalidDomain) {
		t.Fatalf("err = %v, want FailureInvalidDomain", err)
	}
}

func TestPlanRejectsSelfRecursion(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{
			"noop": {Name: "noop"},
		},
		Methods: map[string][]*Method{
			"loop": {{Name: "again", Subtasks: []TaskRef{Primitive("noop"), Compound("loop")}}},
		},
	}
	p := NewPlanner(PlannerConfig{})
	_, err := p.Plan(d, NewState(), []TaskRef{Compound("loop")})
	if !IsFailure(err, FailureInvalidDomain) {
		t.Fatalf("err = %v, want FailureInvalidDomain", err)
	}
}

func TestPlanBudgetExceeded(t *testing.T) {
	p := NewPlanner(PlannerConfig{MaxExpansions: 1})
	_, err := p.Plan(robotDomain(), NewState("at-hall", "key-in-room"), []TaskRef{Compound("fetch-key")})
	if !IsFailure(err, FailureBudgetExceeded) {
		t.Fatalf("err = %v, want FailureBudgetExceeded", err)
	}
}

func TestPlanInvalidDomainUnknownReference(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{},
		Methods: map[string][]*Method{
			"broken": {{Name: "m", Subtasks: []TaskRef{Primitive("missing")}}},
		},
	}
	p := NewPlanner(PlannerConfig{})
	_, err := p.Plan(d, NewState(), []TaskRef{Compound("broken")})
	if !IsFailure(err, FailureInvalidDomain) {
		t.Fatalf("err = %v, want FailureInvalidDomain", err)
	}
}

func TestPlanUnknownGoalTask(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	_, err := p.Plan(robotDomain(), NewState(), []TaskRef{Compound("no-such-task")})
	if !IsFailure(err, FailureInvalidDomain) {
		t.Fatalf("err = %v, want FailureInvalidDomain", err)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	state := NewState("at-hall", "key-in-room")
	goals := []TaskRef{Primitive("open-door"), Compound("fetch-key")}
	first, err := p.Plan(robotDomain(), state, goals)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := p.Plan(robotDomain(), state, goals)
		if err != nil {
			t.Fatalf("Plan failed: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\nfirst: %+v\nagain: %+v", i, first, again)
		}
	}
}

func TestPlanMultipleGoalsAndLayers(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{
			"charge-arm": {Name: "charge-arm", Eff: Effects{Add: []string{"arm-charged"}}},
			"charge-leg": {Name: "charge-leg", Eff: Effects{Add: []string{"leg-charged"}}},
			"walk":       {Name: "walk", Pre: Conditions{Pos: []string{"leg-charged"}}, Eff: Effects{Add: []string{"at-target"}}},
			"wave":       {Name: "wave", Pre: Conditions{Pos: []string{"arm-charged"}}, Eff: Effects{Add: []string{"greeted"}}},
			"celebrate":  {Name: "celebrate", Pre: Conditions{Pos: []string{"at-target", "greeted"}}, Eff: Effects{Add: []string{"done"}}},
		},
	}
	p := NewPlanner(PlannerConfig{})
	goals := []TaskRef{
		Primitive("charge-arm"),
		Primitive("charge-leg"),
		Primitive("walk"),
		Primitive("wave"),
		Primitive("celebrate"),
	}
	plan, err := p.Plan(d, NewState(), goals)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	// charge-arm and charge-leg are independent: same layer.
	// walk depends on charge-leg, wave on charge-arm: next layer.
	// celebrate depends on both: last layer.
	want := [][]int{{0, 1}, {2, 3}, {4}}
	if !reflect.DeepEqual(plan.Layers, want) {
		t.Fatalf("layers = %v, want %v", plan.Layers, want)
	}
}

func TestPlanLayersSeparateConflictingEffects(t *testing.T) {
	d := &Domain{
		Actions: map[string]*Action{
			"raise": {Name: "raise", Eff: Effects{Add: []string{"flag-up"}}},
			"lower": {Name: "lower", Eff: Effects{Del: []string{"flag-up"}}},
		},
	}
	p := NewPlanner(PlannerConfig{})
	plan, err := p.Plan(d, NewState(), []TaskRef{Primitive("raise"), Primitive("lower")})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(plan.Layers) != 2 {
		t.Fatalf("layers = %v, want two layers for conflicting effects", plan.Layers)
	}
}

func TestPlanEveryStepPreconditionsHold(t *testing.T) {
	p := NewPlanner(PlannerConfig{})
	d := robotDomain()
	state := NewState("at-hall", "key-in-room")
	plan, err := p.Plan(d, state, []TaskRef{Compound("fetch-key")})
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	sim := state.Clone()
	for _, step := range plan.Steps {
		a := d.Actions[step.Action]
		if !a.Pre.SatisfiedBy(sim) {
			t.Fatalf("step %d (%s) preconditions %v not satisfied in %v", step.Index, step.Action, a.Pre.Pos, sim.Sorted())
		}
		sim.Apply(a.Eff)
	}
}
