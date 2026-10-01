package taskplan

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// chainDomain is a strictly sequential pipeline: boot -> calibrate ->
// scan -> report, each action enabled by the previous one.
func chainDomain() *Domain {
	return &Domain{
		Actions: map[string]*Action{
			"boot":      {Name: "boot", Eff: Effects{Add: []string{"system-on"}}},
			"calibrate": {Name: "calibrate", Pre: Conditions{Pos: []string{"system-on"}}, Eff: Effects{Add: []string{"calibrated"}}},
			"scan":      {Name: "scan", Pre: Conditions{Pos: []string{"calibrated"}}, Eff: Effects{Add: []string{"scanned"}}},
			"report":    {Name: "report", Pre: Conditions{Pos: []string{"scanned"}}, Eff: Effects{Add: []string{"reported"}}},
		},
	}
}

func chainGoals() []TaskRef {
	return []TaskRef{Primitive("boot"), Primitive("calibrate"), Primitive("scan"), Primitive("report")}
}

// callCounter records how often each action ran.
type callCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCallCounter() *callCounter { return &callCounter{counts: map[string]int{}} }

func (c *callCounter) inc(name string) {
	c.mu.Lock()
	c.counts[name]++
	c.mu.Unlock()
}

func (c *callCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

func executedNames(res *Result) []string {
	out := make([]string, len(res.Executed))
	for i, r := range res.Executed {
		out[i] = r.Action
	}
	return out
}

func TestExecutorCompletesSequentialPlan(t *testing.T) {
	ex := NewExecutor(ExecutorConfig{}, PlannerConfig{})
	res, err := ex.Run(context.Background(), chainDomain(), NewState(), chainGoals(), nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	want := []string{"boot", "calibrate", "scan", "report"}
	if got := executedNames(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("executed = %v, want %v", got, want)
	}
	if !res.FinalState.Has("reported") {
		t.Fatalf("final state %v misses reported", res.FinalState.Sorted())
	}
}

func TestExecutorReplansAfterActionFailure(t *testing.T) {
	calls := newCallCounter()
	var scanFailures int
	runner := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		calls.inc(a.Name)
		if a.Name == "scan" && scanFailures == 0 {
			scanFailures++
			return nil, errors.New("sensor timeout")
		}
		return nil, nil
	})
	ex := NewExecutor(ExecutorConfig{}, PlannerConfig{})
	res, err := ex.Run(context.Background(), chainDomain(), NewState(), chainGoals(), runner)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Replans != 1 {
		t.Fatalf("replans = %d, want 1", res.Replans)
	}
	// Actions before the failure must not run again.
	if got := calls.get("boot"); got != 1 {
		t.Fatalf("boot ran %d times, want 1", got)
	}
	if got := calls.get("calibrate"); got != 1 {
		t.Fatalf("calibrate ran %d times, want 1", got)
	}
	if got := calls.get("scan"); got != 2 {
		t.Fatalf("scan ran %d times, want 2", got)
	}
	if !res.FinalState.Has("reported") {
		t.Fatalf("final state %v misses reported", res.FinalState.Sorted())
	}
}

func TestExecutorReplansAfterEffectsMismatch(t *testing.T) {
	calls := newCallCounter()
	var mismatches int
	runner := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		calls.inc(a.Name)
		if a.Name == "calibrate" && mismatches == 0 {
			mismatches++
			return &Effects{}, nil // observed: nothing happened
		}
		return nil, nil
	})
	ex := NewExecutor(ExecutorConfig{}, PlannerConfig{})
	res, err := ex.Run(context.Background(), chainDomain(), NewState(), chainGoals(), runner)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Replans != 1 {
		t.Fatalf("replans = %d, want 1", res.Replans)
	}
	if got := calls.get("calibrate"); got != 2 {
		t.Fatalf("calibrate ran %d times, want 2", got)
	}
	if !res.FinalState.Has("reported") {
		t.Fatalf("final state %v misses reported", res.FinalState.Sorted())
	}
}

func TestExecutorGivesUpWithDistinguishableFailure(t *testing.T) {
	runner := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		if a.Name == "scan" {
			return nil, errors.New("permanent hardware fault")
		}
		return nil, nil
	})
	ex := NewExecutor(ExecutorConfig{MaxReplans: 2}, PlannerConfig{})
	res, err := ex.Run(context.Background(), chainDomain(), NewState(), chainGoals(), runner)
	if !IsFailure(err, FailureExecutionFailed) {
		t.Fatalf("err = %v, want FailureExecutionFailed", err)
	}
	if got := executedNames(res); !reflect.DeepEqual(got, []string{"boot", "calibrate"}) {
		t.Fatalf("executed = %v, want [boot calibrate]", got)
	}
	if res.FinalState.Has("scanned") {
		t.Fatalf("final state %v must not contain scanned", res.FinalState.Sorted())
	}
}

func TestExecutorResumesFromSnapshot(t *testing.T) {
	store := filepath.Join(t.TempDir(), "progress.json")
	domain := chainDomain()
	goals := chainGoals()

	// First process: scan always fails, the run dies with a snapshot
	// committed after "calibrate".
	failing := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		if a.Name == "scan" {
			return nil, errors.New("process killed")
		}
		return nil, nil
	})
	ex1 := NewExecutor(ExecutorConfig{StorePath: store, MaxReplans: 1}, PlannerConfig{})
	if _, err := ex1.Run(context.Background(), domain, NewState(), goals, failing); !IsFailure(err, FailureExecutionFailed) {
		t.Fatalf("first run err = %v, want FailureExecutionFailed", err)
	}

	// Second process: fresh executor, same store. It must resume after
	// "calibrate" and never re-run boot or calibrate.
	calls := newCallCounter()
	working := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		calls.inc(a.Name)
		return nil, nil
	})
	ex2 := NewExecutor(ExecutorConfig{StorePath: store, MaxReplans: 1}, PlannerConfig{})
	res, err := ex2.Run(context.Background(), domain, NewState(), goals, working)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if got := calls.get("boot"); got != 0 {
		t.Fatalf("boot re-executed %d times after restart", got)
	}
	if got := calls.get("calibrate"); got != 0 {
		t.Fatalf("calibrate re-executed %d times after restart", got)
	}
	if got := executedNames(res); !reflect.DeepEqual(got, []string{"scan", "report"}) {
		t.Fatalf("second run executed = %v, want [scan report]", got)
	}
	if !res.FinalState.Has("reported") {
		t.Fatalf("final state %v misses reported", res.FinalState.Sorted())
	}

	// Third run: snapshot is done, nothing executes again.
	ex3 := NewExecutor(ExecutorConfig{StorePath: store}, PlannerConfig{})
	res3, err := ex3.Run(context.Background(), domain, NewState(), goals, working)
	if err != nil {
		t.Fatalf("third run failed: %v", err)
	}
	if len(res3.Executed) != 0 {
		t.Fatalf("third run executed %v, want nothing", executedNames(res3))
	}
}

// parallelDomain offers three independent preparations and a final
// action requiring all of them.
func parallelDomain() *Domain {
	return &Domain{
		Actions: map[string]*Action{
			"prep-a":   {Name: "prep-a", Eff: Effects{Add: []string{"a-ready"}}},
			"prep-b":   {Name: "prep-b", Eff: Effects{Add: []string{"b-ready"}}},
			"prep-c":   {Name: "prep-c", Eff: Effects{Add: []string{"c-ready"}}},
			"finalize": {Name: "finalize", Pre: Conditions{Pos: []string{"a-ready", "b-ready", "c-ready"}}, Eff: Effects{Add: []string{"done"}}},
		},
	}
}

func parallelGoals() []TaskRef {
	return []TaskRef{Primitive("prep-a"), Primitive("prep-b"), Primitive("prep-c"), Primitive("finalize")}
}

func TestExecutorParallelBranchFailureIsDeterministic(t *testing.T) {
	runScenario := func() (*Result, *callCounter, error) {
		calls := newCallCounter()
		var bFailures int
		var mu sync.Mutex
		runner := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
			calls.inc(a.Name)
			mu.Lock()
			defer mu.Unlock()
			if a.Name == "prep-b" && bFailures == 0 {
				bFailures++
				return nil, errors.New("arm stuck")
			}
			return nil, nil
		})
		ex := NewExecutor(ExecutorConfig{MaxParallel: 3}, PlannerConfig{})
		res, err := ex.Run(context.Background(), parallelDomain(), NewState(), parallelGoals(), runner)
		return res, calls, err
	}

	res1, calls1, err1 := runScenario()
	if err1 != nil {
		t.Fatalf("run failed: %v", err1)
	}
	// Sibling branches of the failed one ran exactly once.
	if got := calls1.get("prep-a"); got != 1 {
		t.Fatalf("prep-a ran %d times, want 1", got)
	}
	if got := calls1.get("prep-c"); got != 1 {
		t.Fatalf("prep-c ran %d times, want 1", got)
	}
	if got := calls1.get("prep-b"); got != 2 {
		t.Fatalf("prep-b ran %d times, want 2", got)
	}
	wantOrder := []string{"prep-a", "prep-c", "prep-b", "finalize"}
	if got := executedNames(res1); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("commit order = %v, want %v", got, wantOrder)
	}
	for _, f := range []string{"a-ready", "b-ready", "c-ready", "done"} {
		if !res1.FinalState.Has(f) {
			t.Fatalf("final state %v misses %s", res1.FinalState.Sorted(), f)
		}
	}

	// The identical scenario must produce the identical outcome.
	res2, _, err2 := runScenario()
	if err2 != nil {
		t.Fatalf("rerun failed: %v", err2)
	}
	if !reflect.DeepEqual(executedNames(res1), executedNames(res2)) {
		t.Fatalf("non-deterministic commit order: %v vs %v", executedNames(res1), executedNames(res2))
	}
	if !res1.FinalState.Equal(res2.FinalState) {
		t.Fatalf("non-deterministic final state: %v vs %v", res1.FinalState.Sorted(), res2.FinalState.Sorted())
	}
}

func TestExecutorMaxParallelCapsConcurrency(t *testing.T) {
	var mu sync.Mutex
	inFlight := 0
	maxSeen := 0
	runner := RunnerFunc(func(ctx context.Context, a Action, s State) (*Effects, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil, nil
	})

	ex := NewExecutor(ExecutorConfig{MaxParallel: 2}, PlannerConfig{})
	res, err := ex.Run(context.Background(), parallelDomain(), NewState(), parallelGoals(), runner)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if maxSeen > 2 {
		t.Fatalf("observed %d concurrent actions, MaxParallel=2 must cap at 2", maxSeen)
	}
	if !res.FinalState.Has("done") {
		t.Fatalf("final state %v misses done", res.FinalState.Sorted())
	}

	// Sequential execution of the same plan yields the same final state.
	exSeq := NewExecutor(ExecutorConfig{MaxParallel: 1}, PlannerConfig{})
	resSeq, err := exSeq.Run(context.Background(), parallelDomain(), NewState(), parallelGoals(), nil)
	if err != nil {
		t.Fatalf("sequential Run failed: %v", err)
	}
	if !res.FinalState.Equal(resSeq.FinalState) {
		t.Fatalf("parallel and sequential outcomes differ: %v vs %v", res.FinalState.Sorted(), resSeq.FinalState.Sorted())
	}
}
