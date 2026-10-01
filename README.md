# task-planner-go

进程内的层次化任务规划（HTN）与执行监控库，面向机器人与自动化系统的任务编排。
调用方以领域描述声明可执行动作、复合任务及其分解方式，规划器生成满足前置条件
约束的动作序列，执行器驱动执行、监控偏差并自动重规划。不依赖任何外部服务。

## 快速上手

```go
domain := &taskplan.Domain{
    Actions: map[string]*taskplan.Action{
        "move-hall-room": {
            Name: "move-hall-room",
            Pre:  taskplan.Conditions{Pos: []string{"at-hall"}},
            Eff:  taskplan.Effects{Add: []string{"at-room"}, Del: []string{"at-hall"}},
        },
        "pick-key": {
            Name: "pick-key",
            Pre:  taskplan.Conditions{Pos: []string{"at-room", "key-in-room"}},
            Eff:  taskplan.Effects{Add: []string{"have-key"}, Del: []string{"key-in-room"}},
        },
    },
    Methods: map[string][]*taskplan.Method{
        "fetch-key": {
            {Name: "optimistic", Subtasks: []taskplan.TaskRef{taskplan.Primitive("pick-key")}},
            {Name: "walk-and-pick", Subtasks: []taskplan.TaskRef{
                taskplan.Primitive("move-hall-room"), taskplan.Primitive("pick-key")}},
        },
    },
}

planner := taskplan.NewPlanner(taskplan.PlannerConfig{})
plan, err := planner.Plan(domain, taskplan.NewState("at-hall", "key-in-room"),
    []taskplan.TaskRef{taskplan.Compound("fetch-key")})
// plan.Actions() == ["move-hall-room", "pick-key"]
// plan.Layers   == [[0], [1]]（有依赖，不能并行）

executor := taskplan.NewExecutor(taskplan.ExecutorConfig{
    MaxParallel: 4,
    StorePath:   "progress.json", // 可选：持久化执行进度
}, taskplan.PlannerConfig{})
res, err := executor.Run(ctx, domain, taskplan.NewState("at-hall", "key-in-room"),
    []taskplan.TaskRef{taskplan.Compound("fetch-key")}, myRunner)
```

## 领域模型

- `State`：当前为真的事实集合，事实为自由格式字符串（如 `"at-hall"`）。
- `Action`：原子动作，`Pre`（正/负前置条件）在执行位置必须满足，`Eff`（增/删效果）
  描述执行后的状态变化。
- `Method`：复合任务的一种分解方式，含可选前置条件 `Pre` 与有序子任务列表
  （子任务可以是动作，也可以是复合任务，允许任意深度嵌套）。
- `Domain`：`Actions` 与 `Methods`（按复合任务名分组，方法按声明顺序候选）。
- 目标：一组 `TaskRef`（`Primitive(name)` 或 `Compound(name)`），一次规划可同时
  给出多个目标，按给定顺序依次达成。

## 规划语义

- **全序前向分解**：规划器从初始状态出发，按目标顺序展开任务；复合任务依次尝试
  其候选方法（先检查方法前置条件），某方法失败时回溯尝试下一方法，并逐层回退到
  上层选择点。
- **逐动作前置条件校验**：每个原子动作被加入计划前，都会在当时模拟状态上校验
  前置条件，保证输出计划中每个动作在其执行位置可执行。
- **递归环拒绝**：`Domain.Validate` 静态检测复合任务分解图中的环（含自环与间接
  环），含环领域以 `FailureInvalidDomain` 拒绝，规划器不会因此无限递归。
- **并行层划分**：`Plan.Layers` 把全序动作序列划分为有序层。同层动作两两无冲突
  ——互不读写对方依赖的事实，且效果不互斥（一方新增、另一方删除同一事实）——
  因此同层动作可以任意顺序并发执行；跨层保持依赖顺序。
- **确定性**：同一领域、同一初始状态、同一目标列表，重复规划产出结构完全相同的
  计划（方法按声明顺序尝试，层划分由全序序列唯一决定）。
- **空计划**：仅当目标列表为空，或目标全部由空子任务方法分解时，计划才为空；
  任何失败都以显式错误返回，不会静默返回空计划。

## 执行语义

- 执行器逐层驱动计划：层内动作全部启动（并发度受 `MaxParallel` 限制）并等待全部
  完成，然后**按计划顺序**提交结果。层是原子提交单元：任一分支失败时，其余在途
  分支继续执行完成（确定的"继续"策略），最终状态与 goroutine 调度时序无关，相同
  场景重复执行得到相同结果。
- `Runner` 返回动作的实际效果：返回 `nil` 表示与声明效果一致；返回不同效果（效果
  偏差）或错误（动作失败）都会触发**重规划**——以观察到的真实状态为起点，对尚未
  执行的任务重新规划。已执行动作对应的任务不会进入重规划，已完成的工作不会重复
  执行；新计划的前置条件对当前状态重新校验，保证与当前状态一致。
- **持久化恢复**：配置 `StorePath` 后，每提交一个动作就原子写入一次快照（状态 +
  剩余任务）。进程重启后用同一 `StorePath` 再次调用 `Run`，从最近提交位置继续：
  已完成动作不重复执行，未执行步骤不丢失。快照标记完成后再次调用立即返回；
  删除快照文件即可重新开始。

## 失败结果分类

所有规划与执行失败都返回 `*taskplan.Failure`，用 `IsFailure(err, kind)` 区分：

| Kind | 含义 |
| --- | --- |
| `FailureInvalidDomain` | 领域不合法：引用未声明任务、动作与复合任务重名、存在递归分解环 |
| `FailureGoalUnreachable` | 目标不可达：所有方法组合都无法从当前状态达成目标 |
| `FailureBudgetExceeded` | 规划超出 `MaxExpansions` 预算（立即返回，不会挂起） |
| `FailureExecutionFailed` | 执行阶段动作持续失败，重规划次数超出 `MaxReplans` |

## 配置项

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `PlannerConfig.MaxExpansions` | `10000` | 规划扩展步数预算，超出返回 `FailureBudgetExceeded` |
| `ExecutorConfig.MaxParallel` | `1`（顺序） | 层内动作的最大并发数；只限并发度，不改变提交结果 |
| `ExecutorConfig.MaxReplans` | `8` | 执行失败后的最大重规划次数，超出返回 `FailureExecutionFailed` |
| `ExecutorConfig.StorePath` | 空（不持久化） | 进度快照文件路径，用于重启恢复 |

## 测试

```sh
go test ./...        # 单元测试
go test -race ./...  # 含竞态检测
```

测试覆盖：分解回退、方法前置条件选择、目标不可达、递归环（直接与间接）、
规划预算、计划确定性、并行层划分、逐动作前置条件校验、执行失败重规划、
效果偏差重规划、重规划预算耗尽、重启恢复、并行分支失败确定性、并发度上限。
