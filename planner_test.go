package taskplanner

import (
	"context"
	"errors"
	"testing"
)

func lit(name string, args ...string) Literal {
	return Literal{Predicate: name, Args: args}
}

func notLit(name string, args ...string) Literal {
	return Literal{Predicate: name, Args: args, Negated: true}
}

func task(name string, args ...string) TaskExpr {
	return TaskExpr{Name: name, Args: args}
}

// deliveryDomain 构造一个配送领域：
//
//	deliver(?to) 有两种分解方式：直送 direct（需要 road_open）或绕行 via_hub（需要 hub_open）。
//	move(?from,?to) 是唯一基元动作。
func deliveryDomain() *Domain {
	d := NewDomain("delivery")
	d.Actions["move"] = &Action{
		Name:   "move",
		Params: []string{"?from", "?to"},
		Pre:    []Literal{lit("at", "?from"), notLit("at", "?to")},
		Add:    []Literal{lit("at", "?to"), lit("visited", "?to")},
		Del:    []Literal{lit("at", "?from")},
	}
	d.Compounds["deliver"] = &CompoundTask{
		Name:   "deliver",
		Params: []string{"?from", "?to"},
		Methods: []Method{
			{
				Name:   "direct",
				Pre:    []Literal{lit("road_open", "?to")},
				Groups: [][]TaskExpr{{task("move", "?from", "?to")}},
			},
			{
				Name: "via_hub",
				Pre:  []Literal{lit("hub_open", "?to")},
				Groups: [][]TaskExpr{
					{task("move", "?from", "hub")},
					{task("move", "hub", "?to")},
				},
			},
		},
	}
	d.Compounds["move_pair"] = &CompoundTask{
		Name:   "move_pair",
		Params: []string{"?fa", "?ta", "?fb", "?tb"},
		Methods: []Method{{
			Name: "parallel",
			Groups: [][]TaskExpr{
				{task("move", "?fa", "?ta"), task("move", "?fb", "?tb")},
			},
		}},
	}
	return d
}

func actionKeys(plan *Plan) [][]string {
	out := make([][]string, len(plan.Waves))
	for i, w := range plan.Waves {
		for _, g := range w.Actions {
			out[i] = append(out[i], g.Key())
		}
	}
	return out
}

func TestPlanMethodFallback(t *testing.T) {
	d := deliveryDomain()
	p := NewPlanner(d, Config{})

	// 直送可用：应选择 direct，只有一个 move 动作。
	plan, final, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base"), lit("road_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionKeys(plan); len(got) != 1 || len(got[0]) != 1 || got[0][0] != "move(base,north)" {
		t.Fatalf("expected direct move(base,north), got %v", got)
	}
	if !final.Has(lit("at", "north")) {
		t.Fatalf("final state missing goal: %v", final.AtomKeys())
	}

	// 直送不可用、绕行可用：第一种分解必须失败并回退到第二种。
	plan, _, err = p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base"), lit("hub_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("expected fallback to via_hub, got error: %v", err)
	}
	got := actionKeys(plan)
	if len(got) != 2 || got[0][0] != "move(base,hub)" || got[1][0] != "move(hub,north)" {
		t.Fatalf("expected via_hub sequence, got %v", got)
	}
}

func TestPlanUnreachable(t *testing.T) {
	d := deliveryDomain()
	p := NewPlanner(d, Config{})

	_, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if KindOf(err) != FailureUnreachable {
		t.Fatalf("expected unreachable, got %v", err)
	}

	// 目标与执行结果矛盾也必须不可达，而不是静默返回。
	_, _, err = p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base"), lit("road_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "base")},
	})
	if KindOf(err) != FailureUnreachable {
		t.Fatalf("expected unreachable for contradictory goal, got %v", err)
	}
}

func TestPlanRecursiveCycleRejected(t *testing.T) {
	d := NewDomain("cycle")
	d.Actions["noop"] = &Action{Name: "noop", Add: []Literal{lit("done")}}
	d.Compounds["loop"] = &CompoundTask{
		Name:   "loop",
		Params: []string{"?x"},
		Methods: []Method{{
			Name:   "recurse",
			Groups: [][]TaskExpr{{task("loop", "?x")}},
		}},
	}
	p := NewPlanner(d, Config{})

	_, _, err := p.Plan(context.Background(), PlanRequest{
		RootTasks: []TaskExpr{task("loop", "a")},
		Goals:     []Literal{lit("done")},
	})
	if KindOf(err) != FailureInvalidDomain {
		t.Fatalf("expected invalid_domain for recursive cycle, got %v", err)
	}

	// 间接环同样必须被检测。
	d2 := NewDomain("cycle2")
	d2.Actions["noop"] = d.Actions["noop"]
	d2.Compounds["a"] = &CompoundTask{
		Name: "a",
		Methods: []Method{{
			Groups: [][]TaskExpr{{task("b")}},
		}},
	}
	d2.Compounds["b"] = &CompoundTask{
		Name: "b",
		Methods: []Method{{
			Groups: [][]TaskExpr{{task("a")}},
		}},
	}
	if err := d2.Validate(); KindOf(err) != FailureInvalidDomain {
		t.Fatalf("expected invalid_domain for indirect cycle, got %v", err)
	}
}

func TestPlanBudgetExceeded(t *testing.T) {
	// 构造必须尝试大量分解的场景：direct 前置条件永不满足，
	// via_hub 中第一步成功后第二步前置不满足，从而反复回溯并耗尽预算。
	d := deliveryDomain()
	p := NewPlanner(d, Config{Budget: 1})

	_, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base"), lit("hub_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if KindOf(err) != FailureBudgetExceeded {
		t.Fatalf("expected budget_exceeded, got %v", err)
	}
}

func TestPlanMultipleGoalsAndParallel(t *testing.T) {
	d := deliveryDomain()
	// 一次同时配送 north 与 south，且两者独立，可在同一并行波内执行。
	p := NewPlanner(d, Config{MaxParallel: 4})
	plan, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{
			lit("at", "north"), lit("at", "south"),
		},
		RootTasks: []TaskExpr{task("move_pair", "north", "hub", "south", "east")},
		Goals:     []Literal{lit("at", "hub"), lit("at", "east")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0].Actions) != 2 {
		t.Fatalf("expected single parallel wave with 2 actions, got %v", actionKeys(plan))
	}

	// MaxParallel=1 时同样的计划必须被串行化。
	p1 := NewPlanner(d, Config{MaxParallel: 1})
	plan1, _, err := p1.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{
			lit("at", "north"), lit("at", "south"),
		},
		RootTasks: []TaskExpr{task("move_pair", "north", "hub", "south", "east")},
		Goals:     []Literal{lit("at", "hub"), lit("at", "east")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan1.Waves) != 2 {
		t.Fatalf("expected 2 serialized waves with MaxParallel=1, got %v", actionKeys(plan1))
	}
}

func TestPlanConflictingActionsNotParallel(t *testing.T) {
	d := deliveryDomain()
	p := NewPlanner(d, Config{MaxParallel: 4})
	// 两个动作都读写 at(base)：存在冲突，必须串行。
	plan, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base"), lit("road_open", "hub"), lit("road_open", "north")},
		RootTasks: []TaskExpr{
			task("move", "base", "hub"),
		},
		Goals: []Literal{lit("at", "hub")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Waves) != 1 || len(plan.Waves[0].Actions) != 1 {
		t.Fatalf("expected single action, got %v", actionKeys(plan))
	}

	// 顺序移动 base->hub->north：第二动作依赖第一动作效果，必须在不同波中。
	plan, _, err = p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{
			lit("at", "base"),
			lit("road_open", "hub"), lit("road_open", "north"),
			lit("hub_open", "north"),
		},
		RootTasks: []TaskExpr{
			task("move", "base", "hub"),
			task("move", "hub", "north"),
		},
		Goals: []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Waves) != 2 {
		t.Fatalf("dependent actions must be in separate waves, got %v", actionKeys(plan))
	}
}

func TestPlanDeterministic(t *testing.T) {
	d := deliveryDomain()
	req := PlanRequest{
		InitialState: []Literal{lit("at", "a"), lit("at", "b")},
		RootTasks: []TaskExpr{
			task("move_pair", "a", "c", "b", "d"),
			task("move_pair", "c", "e", "d", "f"),
		},
		Goals: []Literal{lit("at", "e"), lit("at", "f")},
	}
	var prev [][]string
	for i := 0; i < 5; i++ {
		p := NewPlanner(d, Config{})
		plan, _, err := p.Plan(context.Background(), req)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		cur := actionKeys(plan)
		if i > 0 && !equalKeyWaves(prev, cur) {
			t.Fatalf("non-deterministic plans:\n%v\n%v", prev, cur)
		}
		prev = cur
	}
}

func TestPlanInvalidRequestDistinction(t *testing.T) {
	d := deliveryDomain()
	p := NewPlanner(d, Config{})

	_, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "base")},
		RootTasks:    []TaskExpr{task("unknown", "x")},
		Goals:        []Literal{lit("at", "x")},
	})
	if KindOf(err) != FailureInvalidDomain {
		t.Fatalf("expected invalid_domain, got %v", err)
	}

	var pe *PlanError
	if !errors.As(err, &pe) || pe.Kind != FailureInvalidDomain {
		t.Fatalf("expected *PlanError with distinct kind, got %T %v", err, err)
	}
}

// 方法组内两个子任务若一个依赖另一个的效果（跨子任务冲突），必须整体顺序化。
func TestPlanCrossChildDependencySerializes(t *testing.T) {
	d := NewDomain("dep")
	d.Actions["a"] = &Action{Name: "a", Add: []Literal{lit("flag")}}
	d.Actions["b"] = &Action{Name: "b", Pre: []Literal{lit("flag")}, Add: []Literal{lit("done")}}
	d.Compounds["ab"] = &CompoundTask{
		Name: "ab",
		Methods: []Method{{
			Name:   "g",
			Groups: [][]TaskExpr{{task("a"), task("b")}},
		}},
	}
	p := NewPlanner(d, Config{MaxParallel: 4})
	plan, _, err := p.Plan(context.Background(), PlanRequest{
		RootTasks: []TaskExpr{task("ab")},
		Goals:     []Literal{lit("done")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionKeys(plan); len(got) != 2 {
		t.Fatalf("dependent children must serialize into 2 waves, got %v", got)
	}
}

// 领域中引用未知任务、参数元数不符时，必须返回 invalid_domain。
func TestInvalidDomainReferences(t *testing.T) {
	d := NewDomain("bad")
	d.Actions["a"] = &Action{Name: "a", Add: []Literal{lit("done")}}
	d.Compounds["c"] = &CompoundTask{
		Name: "c",
		Methods: []Method{{
			Groups: [][]TaskExpr{{task("missing")}},
		}},
	}
	if err := d.Validate(); KindOf(err) != FailureInvalidDomain {
		t.Fatalf("expected invalid_domain for unknown subtask, got %v", err)
	}

	d2 := NewDomain("bad2")
	d2.Actions["a"] = &Action{Name: "a", Params: []string{"?x"}, Add: []Literal{lit("done")}}
	d2.Compounds["c"] = &CompoundTask{
		Name: "c",
		Methods: []Method{{
			Groups: [][]TaskExpr{{task("a")}},
		}},
	}
	if err := d2.Validate(); KindOf(err) != FailureInvalidDomain {
		t.Fatalf("expected invalid_domain for arg arity mismatch, got %v", err)
	}
}

// 无待执行任务且目标已满足时，空计划是合法结果。
func TestPlanEmptyWhenGoalsAlreadyMet(t *testing.T) {
	d := deliveryDomain()
	p := NewPlanner(d, Config{})
	plan, _, err := p.Plan(context.Background(), PlanRequest{
		InitialState: []Literal{lit("at", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Waves) != 0 {
		t.Fatalf("expected empty plan, got %v", actionKeys(plan))
	}
}

func equalKeyWaves(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}
