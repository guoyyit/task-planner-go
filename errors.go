package taskplan

import (
	"errors"
	"fmt"
)

// FailureKind classifies the ways planning or execution can fail so that
// callers can distinguish them programmatically.
type FailureKind int

const (
	// FailureInvalidDomain means the domain description is malformed:
	// unknown task references, name collisions, or recursive
	// decomposition cycles.
	FailureInvalidDomain FailureKind = iota + 1
	// FailureGoalUnreachable means no decomposition of the goals yields
	// an executable plan from the current state.
	FailureGoalUnreachable
	// FailureBudgetExceeded means the planner exhausted
	// PlannerConfig.MaxExpansions before finding a plan.
	FailureBudgetExceeded
	// FailureExecutionFailed means the executor could not complete the
	// plan: an action kept failing and the replan budget
	// (ExecutorConfig.MaxReplans) was exhausted.
	FailureExecutionFailed
)

func (k FailureKind) String() string {
	switch k {
	case FailureInvalidDomain:
		return "invalid domain"
	case FailureGoalUnreachable:
		return "goal unreachable"
	case FailureBudgetExceeded:
		return "planning budget exceeded"
	case FailureExecutionFailed:
		return "execution failed"
	default:
		return "unknown failure"
	}
}

// Failure is the error type returned by the planner and the executor.
// Use IsFailure to classify it.
type Failure struct {
	Kind    FailureKind
	Message string
}

func (f *Failure) Error() string {
	return fmt.Sprintf("taskplan: %s: %s", f.Kind, f.Message)
}

// IsFailure reports whether err is a *Failure with the given kind.
func IsFailure(err error, kind FailureKind) bool {
	var f *Failure
	if !errors.As(err, &f) {
		return false
	}
	return f.Kind == kind
}

func invalidDomainf(format string, args ...any) *Failure {
	return &Failure{Kind: FailureInvalidDomain, Message: fmt.Sprintf(format, args...)}
}

func unreachablef(format string, args ...any) *Failure {
	return &Failure{Kind: FailureGoalUnreachable, Message: fmt.Sprintf(format, args...)}
}

func budgetf(format string, args ...any) *Failure {
	return &Failure{Kind: FailureBudgetExceeded, Message: fmt.Sprintf(format, args...)}
}

func execf(format string, args ...any) *Failure {
	return &Failure{Kind: FailureExecutionFailed, Message: fmt.Sprintf(format, args...)}
}
