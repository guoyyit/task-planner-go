package taskplanner_test

import (
	"context"
	"fmt"

	tp "github.com/guoyyit/task-planner-go"
)

// ExampleOverview 展示领域声明、层次化规划（含方法回退）与执行监控的完整用法。
func Example() {
	d := tp.NewDomain("factory")

	d.Actions["pick"] = &tp.Action{
		Name:   "pick",
		Params: []string{"?what"},
		Pre:    []tp.Literal{tp.Literal{Predicate: "hand_empty"}},
		Add:    []tp.Literal{tp.Literal{Predicate: "holding", Args: []string{"?what"}}},
		Del:    []tp.Literal{tp.Literal{Predicate: "hand_empty"}},
	}
	d.Actions["place"] = &tp.Action{
		Name:   "place",
		Params: []string{"?what", "?where"},
		Pre:    []tp.Literal{tp.Literal{Predicate: "holding", Args: []string{"?what"}}},
		Add: []tp.Literal{
			tp.Literal{Predicate: "at", Args: []string{"?what", "?where"}},
			tp.Literal{Predicate: "hand_empty"},
		},
		Del: []tp.Literal{tp.Literal{Predicate: "holding", Args: []string{"?what"}}},
	}

	// 复合任务有两种分解方式：优先传送带，不可用时回退为人工搬运（pick + place）。
	d.Compounds["deliver"] = &tp.CompoundTask{
		Name:   "deliver",
		Params: []string{"?what", "?where"},
		Methods: []tp.Method{
			{
				Name: "via_belt",
				Pre:  []tp.Literal{tp.Literal{Predicate: "belt_on", Args: []string{"?where"}}},
				Groups: [][]tp.TaskExpr{
					{{Name: "pick", Args: []string{"?what"}}},
					{{Name: "place", Args: []string{"?what", "?where"}}},
				},
			},
			{
				Name: "manual",
				Pre:  []tp.Literal{tp.Literal{Predicate: "manual_allowed"}},
				Groups: [][]tp.TaskExpr{
					{{Name: "pick", Args: []string{"?what"}}},
					{{Name: "place", Args: []string{"?what", "?where"}}},
				},
			},
		},
	}

	planner := tp.NewPlanner(d, tp.Config{Budget: 100, MaxParallel: 4})
	plan, final, err := planner.Plan(context.Background(), tp.PlanRequest{
		InitialState: []tp.Literal{
			{Predicate: "hand_empty"},
			{Predicate: "manual_allowed"},
			{Predicate: "part_on", Args: []string{"table"}},
		},
		RootTasks: []tp.TaskExpr{{Name: "deliver", Args: []string{"widget", "bin"}}},
		Goals: []tp.Literal{
			{Predicate: "at", Args: []string{"widget", "bin"}},
			{Predicate: "hand_empty"},
		},
	})
	if err != nil {
		fmt.Println("plan error:", tp.KindOf(err))
		return
	}

	fmt.Println("waves:", len(plan.Waves))
	fmt.Println("goal met:", final.Has(tp.Literal{Predicate: "at", Args: []string{"widget", "bin"}}))

	exec := tp.NewExecutor(d, tp.Config{MaxReplans: 3, FailurePolicy: tp.PolicyStopOnError},
		func(ctx context.Context, g *tp.GroundAction) (*tp.ActionResult, error) {
			return &tp.ActionResult{AsExpected: true}, nil
		})
	report, err := exec.Run(context.Background(), tp.PlanRequest{
		ID:           "factory-1",
		InitialState: planInitial(),
		RootTasks:    []tp.TaskExpr{{Name: "deliver", Args: []string{"widget", "bin"}}},
		Goals: []tp.Literal{
			{Predicate: "at", Args: []string{"widget", "bin"}},
			{Predicate: "hand_empty"},
		},
	})
	if err != nil {
		fmt.Println("exec error:", tp.KindOf(err))
		return
	}
	fmt.Println("exec status:", report.Status, "actions:", len(report.Executed))

	// Output:
	// waves: 2
	// goal met: true
	// exec status: complete actions: 2
}

func planInitial() []tp.Literal {
	return []tp.Literal{
		{Predicate: "hand_empty"},
		{Predicate: "manual_allowed"},
	}
}
