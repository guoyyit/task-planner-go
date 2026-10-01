package taskplanner

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type execCall struct {
	key string
}

func okHandler(calls *[]execCall, mu *sync.Mutex, failOnce map[string]bool) ActionHandler {
	return func(ctx context.Context, g *GroundAction) (*ActionResult, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, execCall{key: g.Key()})
		if failOnce[g.Key()] {
			delete(failOnce, g.Key())
			return nil, errors.New("simulated actuator failure: " + g.Key())
		}
		return &ActionResult{AsExpected: true}, nil
	}
}

func countKey(calls []execCall, key string) int {
	n := 0
	for _, c := range calls {
		if c.key == key {
			n++
		}
	}
	return n
}

// 动作执行失败后，必须从当前状态重新分解并回退到另一种分解方式；已执行动作不得重复执行。
func TestExecuteFailureTriggersReplan(t *testing.T) {
	d := deliveryDomain()
	var calls []execCall
	var mu sync.Mutex

	exec := NewExecutor(d, Config{FailurePolicy: PolicyStopOnError, MaxReplans: 3},
		okHandler(&calls, &mu, map[string]bool{"move(base,north)": true}))

	report, err := exec.Run(context.Background(), PlanRequest{
		ID:           "replan-1",
		InitialState: []Literal{lit("at", "base"), lit("road_open", "north"), lit("hub_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("execution should recover via replan, got %v", err)
	}
	if report.Replans != 1 {
		t.Fatalf("expected exactly 1 replan, got %d", report.Replans)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := countKey(calls, "move(base,north)"); n != 1 {
		t.Fatalf("failed action must not be retried, got %d attempts", n)
	}
	if n := countKey(calls, "move(base,hub)"); n != 1 {
		t.Fatalf("expected move(base,hub) once, got %d", n)
	}
	if n := countKey(calls, "move(hub,north)"); n != 1 {
		t.Fatalf("expected move(hub,north) once, got %d", n)
	}
	if !report.FinalState.Has(lit("at", "north")) {
		t.Fatalf("final state must reach goal, got %v", report.FinalState.AtomKeys())
	}
}

// 所有重规划路径都失败时，必须返回 execution_failed，而不是挂起。
func TestExecuteReplanBudgetExhausted(t *testing.T) {
	d := deliveryDomain()
	var calls []execCall
	var mu sync.Mutex

	alwaysFail := func(ctx context.Context, g *GroundAction) (*ActionResult, error) {
		mu.Lock()
		calls = append(calls, execCall{key: g.Key()})
		mu.Unlock()
		return nil, errors.New("permanent failure: " + g.Key())
	}
	exec := NewExecutor(d, Config{FailurePolicy: PolicyStopOnError, MaxReplans: 2}, alwaysFail)

	_, err := exec.Run(context.Background(), PlanRequest{
		ID:           "replan-exhaust",
		InitialState: []Literal{lit("at", "base"), lit("road_open", "north"), lit("hub_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if KindOf(err) != FailureExecution {
		t.Fatalf("expected execution_failed, got %v", err)
	}
}

// 实际效果与预期不符时，基于观察到的状态重新分解剩余任务。
func TestExecuteDiscrepancyReplan(t *testing.T) {
	d := deliveryDomain()
	var calls []execCall
	var mu sync.Mutex

	handler := func(ctx context.Context, g *GroundAction) (*ActionResult, error) {
		mu.Lock()
		calls = append(calls, execCall{key: g.Key()})
		mu.Unlock()
		if g.Key() == "move(base,hub)" && countKey(calls, "move(base,hub)") == 1 {
			// 预期移动到 hub，实际停留在 base：用空观察效果覆盖声明效果。
			return &ActionResult{AsExpected: false}, nil
		}
		return &ActionResult{AsExpected: true}, nil
	}
	exec := NewExecutor(d, Config{FailurePolicy: PolicyStopOnError, MaxReplans: 3}, handler)

	report, err := exec.Run(context.Background(), PlanRequest{
		ID:           "discrepancy-1",
		InitialState: []Literal{lit("at", "base"), lit("hub_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	})
	if err != nil {
		t.Fatalf("expected recovery from discrepancy, got %v", err)
	}
	if report.Replans == 0 {
		t.Fatalf("discrepancy must trigger replan")
	}
	mu.Lock()
	defer mu.Unlock()
	if countKey(calls, "move(base,hub)") < 2 {
		t.Fatalf("expected re-execution after discrepancy, calls=%v", calls)
	}
	if !report.FinalState.Has(lit("at", "north")) {
		t.Fatalf("goal must be reached, got %v", report.FinalState.AtomKeys())
	}
}

// 并行分支失败（stop）：其余在途分支被停止，本波效果整体不提交；
// 效果按动作键确定序提交，因此最终状态与动作执行次数在重复运行下完全可预期。
func TestExecuteParallelFailureStopPolicy(t *testing.T) {
	type runSummary struct {
		replans int
		final   []string
		counts  map[string]int
	}
	runOnce := func() runSummary {
		d := deliveryDomain()
		var calls []execCall
		var mu sync.Mutex
		exec := NewExecutor(d, Config{FailurePolicy: PolicyStopOnError, MaxParallel: 4, MaxReplans: 3},
			okHandler(&calls, &mu, map[string]bool{"move(b,d)": true}))

		report, err := exec.Run(context.Background(), PlanRequest{
			ID:           "parallel-stop",
			InitialState: []Literal{lit("at", "a"), lit("at", "b")},
			RootTasks:    []TaskExpr{task("move_pair", "a", "c", "b", "d")},
			Goals:        []Literal{lit("at", "c"), lit("at", "d")},
		})
		if err != nil {
			t.Fatalf("stop policy should recover after replan, got %v", err)
		}
		if report.Replans != 1 {
			t.Fatalf("expected 1 replan, got %d", report.Replans)
		}
		if !report.FinalState.Has(lit("at", "c")) || !report.FinalState.Has(lit("at", "d")) {
			t.Fatalf("both goals must be reached, got %v", report.FinalState.AtomKeys())
		}
		mu.Lock()
		counts := map[string]int{}
		for _, c := range calls {
			counts[c.key]++
		}
		mu.Unlock()
		if counts["move(b,d)"] != 2 {
			t.Fatalf("failed branch must be retried exactly once on replan, got %d (%v)", counts["move(b,d)"], calls)
		}
		if counts["move(a,c)"] != 2 {
			t.Fatalf("stopped sibling wave is re-executed on replan, got %d (%v)", counts["move(a,c)"], calls)
		}
		return runSummary{replans: report.Replans, final: report.FinalState.AtomKeys(), counts: counts}
	}

	first := runOnce()
	for i := 0; i < 3; i++ {
		other := runOnce()
		if other.replans != first.replans {
			t.Fatalf("non-deterministic replan count: %d vs %d", other.replans, first.replans)
		}
		if !equalStringSlices(other.final, first.final) {
			t.Fatalf("non-deterministic final state: %v vs %v", other.final, first.final)
		}
		for k, v := range first.counts {
			if other.counts[k] != v {
				t.Fatalf("non-deterministic action count for %s: %d vs %d", k, other.counts[k], v)
			}
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 并行分支失败（continue）：成功分支效果提交，失败分支重跑，最终状态仍可预期。
func TestExecuteParallelFailureContinuePolicy(t *testing.T) {
	d := deliveryDomain()
	var calls []execCall
	var mu sync.Mutex
	exec := NewExecutor(d, Config{FailurePolicy: PolicyContinue, MaxParallel: 4, MaxReplans: 3},
		okHandler(&calls, &mu, map[string]bool{"move(b,d)": true}))

	report, err := exec.Run(context.Background(), PlanRequest{
		ID:           "parallel-continue",
		InitialState: []Literal{lit("at", "a"), lit("at", "b")},
		RootTasks:    []TaskExpr{task("move_pair", "a", "c", "b", "d")},
		Goals:        []Literal{lit("at", "c"), lit("at", "d")},
	})
	if err != nil {
		t.Fatalf("continue policy should recover, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := countKey(calls, "move(a,c)"); n != 1 {
		t.Fatalf("successful branch must execute exactly once, got %d", n)
	}
	if n := countKey(calls, "move(b,d)"); n != 2 {
		t.Fatalf("failed branch must be retried once, got %d", n)
	}
	if !report.FinalState.Has(lit("at", "c")) || !report.FinalState.Has(lit("at", "d")) {
		t.Fatalf("both goals must be reached, got %v", report.FinalState.AtomKeys())
	}
}

// 进程中断后重启：从最近一次已提交位置恢复，不重复执行已完成动作，不丢失未执行步骤。
func TestExecuteRestartRecovery(t *testing.T) {
	d := deliveryDomain()
	dir := t.TempDir()

	// 第一次运行：第二波动作永久失败且禁止重规划，运行中断并返回错误，
	// 但第一波（base->hub）已作为最近提交位置持久化。
	exec1 := NewExecutor(d, Config{PersistDir: dir, MaxReplans: -1},
		func(ctx context.Context, g *GroundAction) (*ActionResult, error) {
			if g.Key() == "move(hub,north)" {
				return nil, errors.New("process killed mid-wave")
			}
			return &ActionResult{AsExpected: true}, nil
		})

	req := PlanRequest{
		ID: "restart-1",
		InitialState: []Literal{
			lit("at", "base"),
			lit("hub_open", "north"),
		},
		RootTasks: []TaskExpr{
			task("deliver", "base", "north"),
		},
		Goals: []Literal{lit("at", "north")},
	}
	_, err := exec1.Run(context.Background(), req)
	if KindOf(err) != FailureExecution {
		t.Fatalf("expected interrupted run to fail with execution_failed, got %v", err)
	}

	// 模拟进程重启：新执行器从 journal 恢复，已提交动作不得重复执行。
	var calls2 []execCall
	var mu2 sync.Mutex
	exec2 := NewExecutor(d, Config{PersistDir: dir, MaxReplans: -1},
		okHandler(&calls2, &mu2, nil))
	report, err := exec2.Resume(context.Background(), "restart-1")
	if err != nil {
		t.Fatalf("resume should succeed, got %v", err)
	}
	if !report.FinalState.Has(lit("at", "north")) {
		t.Fatalf("goal must be reached after resume, got %v", report.FinalState.AtomKeys())
	}
	mu2.Lock()
	defer mu2.Unlock()
	if n := countKey(calls2, "move(base,hub)"); n != 0 {
		t.Fatalf("committed move(base,hub) re-executed after restart: %d", n)
	}
	if n := countKey(calls2, "move(hub,north)"); n != 1 {
		t.Fatalf("uncommitted wave must be re-executed once after resume, got %d", n)
	}
}

// 已完成的 journal 再次执行必须幂等：直接恢复完成状态，不再驱动任何动作。
func TestExecuteCompletedJournalIdempotent(t *testing.T) {
	d := deliveryDomain()
	dir := t.TempDir()

	var calls []execCall
	var mu sync.Mutex
	exec := NewExecutor(d, Config{PersistDir: dir, MaxReplans: 1},
		okHandler(&calls, &mu, nil))
	req := PlanRequest{
		ID:           "done-1",
		InitialState: []Literal{lit("at", "base"), lit("road_open", "north")},
		RootTasks:    []TaskExpr{task("deliver", "base", "north")},
		Goals:        []Literal{lit("at", "north")},
	}
	if _, err := exec.Run(context.Background(), req); err != nil {
		t.Fatalf("first run: %v", err)
	}

	var calls2 []execCall
	var mu2 sync.Mutex
	exec2 := NewExecutor(d, Config{PersistDir: dir},
		okHandler(&calls2, &mu2, nil))
	report, err := exec2.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if report.Status != "complete" || len(calls2) != 0 {
		t.Fatalf("completed journal must resume idempotently, status=%s calls=%v", report.Status, calls2)
	}
}
