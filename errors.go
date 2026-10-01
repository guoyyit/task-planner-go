package taskplanner

import "errors"

// FailureKind 是规划与执行失败的可区分分类。
type FailureKind string

const (
	// FailureInvalidDomain 领域描述本身不合法（未知引用、参数不匹配、递归环等）。
	FailureInvalidDomain FailureKind = "invalid_domain"
	// FailureUnreachable 目标在当前领域与初始状态下不可达。
	FailureUnreachable FailureKind = "unreachable"
	// FailureBudgetExceeded 规划超出预算（分解/展开节点数上限）。
	FailureBudgetExceeded FailureKind = "budget_exceeded"
	// FailureExecution 动作执行失败且重规划次数耗尽或无法恢复。
	FailureExecution FailureKind = "execution_failed"
)

// PlanError 携带可区分的失败分类。
type PlanError struct {
	Kind    FailureKind
	Message string
	Cause   error
}

func (e *PlanError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return string(e.Kind) + ": " + e.Message
	}
	return string(e.Kind)
}

func (e *PlanError) Unwrap() error {
	return e.Cause
}

// KindOf 返回错误对应的失败分类；非 PlanError 返回空字符串。
func KindOf(err error) FailureKind {
	var pe *PlanError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return ""
}
