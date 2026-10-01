# task-planner-go

进程内的层次化任务规划（HTN）与执行监控库，面向机器人 / 自动化系统的任务编排。
调用方通过**领域描述**声明基元动作、复合任务及其多种分解方式、前置条件与效果，
再给出初始状态、根任务序列与目标；规划器据此生成由“可并行动作集合”组成的计划，
执行器逐步驱动动作、应用效果，并在失败或实际效果偏离时基于当前状态自动重新规划。

全部规划与执行都在本进程内完成，不依赖任何外部服务。

## 核心概念

- `Literal`：谓词字面量，如 `at(robot, depot)`；`Negated` 表示否定（用于前置条件）。
- `State`：封闭世界假设下的一组正事实集合。
- `Action`：基元动作（操作符），含形参、前置条件 `Pre`、增删效果 `Add/Del`。
- `CompoundTask`：复合任务，含形参与一个或多个 `Method`（分解方式）。
- `Method`：前置条件满足时，把复合任务分解为**若干有序任务组** `Groups`；
  组与组之间严格顺序，同一组内的子任务允许并行。
- `TaskExpr`：对动作或复合任务的一次引用，实参中以 `?x` 开头者引用父任务形参，
  其余为常量。分解递归进行，但**不允许出现递归分解环**。
- `PlanRequest`：`InitialState` + 顺序的 `RootTasks` + 需同时满足的 `Goals`。
- `Plan`：有序的 `Wave` 序列；每个 `Wave` 是一个可并行执行的动作集合，
  集合内任意两个动作之间都不存在依赖或冲突效果。

## 规划语义

1. **层次化分解与回退**：规划器对每个复合任务按 `Methods` 的声明顺序尝试分解，
   某一方法前置不满足、子任务无法展开或展开后无法达成目标时，回溯并尝试下一方法；
   失败会逐层回退到上层复合任务。
2. **逐动作前置校验**：基元动作只在当前状态满足其全部前置条件时才能被采用；
   计划产出后还会做一次端到端复验，保证计划中每个动作在其执行位置都满足前置条件。
3. **多目标**：`Goals` 是合取式，终态必须同时满足全部目标字面量。
4. **并行划分**：同一方法组内、相互无冲突的子任务被合并到同一并行波。
   - 冲突判定：只要一个动作的写集合（`Add/Del`）触及另一个动作的读集合（`Pre`）
     或写集合，二者就不能同波执行。
   - 一旦组内存在跨子任务冲突，整组退化为按声明顺序执行。
   - 单个波内动作数量受 `MaxParallel` 上限约束，超出上限的动作进入后续波。
5. **确定性**：方法、子任务、动作的枚举顺序以及同波内的提交顺序均按声明序 /
   动作实例键确定；相同领域与相同目标重复规划产出结构相同的计划。
6. **预算**：每次“尝试一种复合任务分解”消耗一次预算；预算耗尽立即返回
   `budget_exceeded`，不会挂起。

## 失败结果分类

所有规划 / 执行错误均为 `*PlanError`，通过 `KindOf(err)` 取 `FailureKind`，
四类失败互不混淆：

| `FailureKind`        | 触发场景 |
|----------------------|----------|
| `invalid_domain`     | 领域不合法：动作/复合任务重名、参数元数不符、未知任务引用、未绑定变量、谓词名为空，或复合任务分解依赖图存在递归环（含自环与间接环）。 |
| `unreachable`        | 领域合法，但在初始状态下没有任何分解路径能同时满足全部目标（也用于目标已部分执行后基于当前状态仍无法达成）。 |
| `budget_exceeded`    | 复合任务分解尝试次数超过 `Budget`。 |
| `execution_failed`   | 动作执行持续失败或持续偏离，且重规划次数（`MaxReplans`）已耗尽。 |

不会静默返回空计划：目标不可达时返回 `unreachable`；仅当无任何待执行任务且
目标已满足时才得到（合法的）空计划。

## 执行与重规划

执行器以“高层任务组 → 有序并行波 → 波内并发动作”三级驱动：

- 每次展开队首高层任务组得到一组波，随后逐波执行；波内动作以 `MaxParallel`
  为上限并发驱动，效果按动作实例键的确定顺序提交。
- **波级提交 + 持久化**：波开始前写 `wave_started`，波全部成功并提交效果后写
  `wave_committed`（临时文件 + 原子 rename）。
- **动作失败 / 偏差重规划**：
  - 组的第一个波失败（stop 策略）时尚无已提交效果，丢弃本次展开、封禁失败动作，
    在高层组上从当前状态重新分解，从而回退到其它分解方式；已执行动作不会重复执行。
  - 后续波失败或进程重启时，高层前缀效果已提交，直接重放剩余已展开波。
  - `ActionResult.AsExpected == false` 时按 `ObservedAdd/ObservedDel` 应用实际
    观察到的状态，并在该真实状态上重新分解当前高层组。
- **并行分支失败策略** `FailurePolicy`：
  - `stop`（默认）：任一分支失败即取消尚未启动 / 在途的调用，本波效果整体不提交，
    随后重规划。最终状态只取决于确定的提交顺序与重规划结果，可重复复现。
  - `continue`：等待同波所有动作完成，提交成功分支的效果，失败动作作为原始任务
    在当前状态单独重试，再继续剩余波。

## 持久化与重启恢复

设置 `Config.PersistDir` 后，每个计划以 `<planID>.json` 保存进度快照，内容包括
当前状态、剩余高层任务网络、当前组已展开的剩余波、已封禁动作、重规划次数与状态。

- `Executor.Run`：若目录中已存在同 ID 计划，则自动转入恢复流程。
- `Executor.Resume(planID)`：按 ID 恢复；`planID` 为空时恢复最近修改的计划。
- 重启后从最近一次已提交位置继续：已提交波不会重复执行，尚未提交的在途波整体
  重跑，未执行步骤不会丢失。已完成计划再次执行是幂等的。
- 封禁集合属于进程内的重试控制状态，重启时清空，避免把瞬时故障固化为永久不可达。

## 配置项

`Config` 字段（零值由构造函数填充默认值）：

| 字段 | 默认值 | 含义 |
|------|--------|------|
| `Budget` | `1000` | 复合任务分解尝试次数上限，`<=0` 用默认值；超出返回 `budget_exceeded`。 |
| `MaxParallel` | `8` | 单个并行波内允许的最大并发动作数，同时也是执行器并发度。 |
| `MaxReplans` | `4` | 执行中允许的重规划次数；`-1` 表示禁止任何重规划（首次失败即返回 `execution_failed`）。 |
| `FailurePolicy` | `stop` | `stop` 或 `continue`，定义并行分支失败时其余在途分支的处置策略。 |
| `PersistDir` | 空（不持久化） | 进度 journal 的落盘目录。 |

## 快速上手

```go
planner := taskplanner.NewPlanner(domain, taskplanner.Config{Budget: 100})
plan, finalState, err := planner.Plan(ctx, taskplanner.PlanRequest{
    InitialState: initFacts,
    RootTasks:    []taskplanner.TaskExpr{{Name: "deliver", Args: []string{"widget", "bin"}}},
    Goals:        []taskplanner.Literal{{Predicate: "at", Args: []string{"widget", "bin"}}},
})

exec := taskplanner.NewExecutor(domain, cfg, func(ctx context.Context, a *taskplanner.GroundAction) (*taskplanner.ActionResult, error) {
    // 在此真正驱动机器人 / 设备动作
    return &taskplanner.ActionResult{AsExpected: true}, nil
})
report, err := exec.Run(ctx, req)              // 执行并自动重规划、持久化
report, err = exec.Resume(ctx, req.ID)        // 进程重启后恢复
```

完整可运行示例见 `example_test.go`，语义与失败分类覆盖见 `planner_test.go`、
`executor_test.go`。
