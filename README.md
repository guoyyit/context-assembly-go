# context-assembly-go

在推理请求处理链路上提供**可配置、可复现**的上下文组装能力。调用方按请求声明输入片段，
组装器按统一规则计算占用、处理冲突、压缩长会话，并在不超预算的前提下尽量保留有效内容。
摘要生成完全由可替换的本地组件完成，不依赖外部服务。

## 快速开始

```go
a := assembly.NewAssembler(
    assembly.WithBudgetPolicy(assembly.BudgetPolicy{
        Global:  assembly.Budget{MaxTokens: 4096, ReserveOutput: 1024},
        Session: assembly.Budget{MaxTokens: 2048},
    }),
    assembly.WithCache(assembly.NewCache(128)),
)

sess := a.Session("conversation-1")
sess.Upsert(assembly.Fragment{
    ID: "msg-1", Source: "chat", Priority: 1, Version: 1,
    CreatedAt: time.Now(), Content: "用户最早的问题……",
})

res, err := a.Assemble(ctx, assembly.Request{
    SessionID: "conversation-1",
    Fragments: []assembly.Fragment{ /* 本次请求的实时素材 */ },
    Budget:    assembly.Budget{MaxTokens: 1024}, // 请求级偏好，可选
})
// res.Content 即组装后的上下文，绝不超出预算。
```

## 预算语义

`Budget{MaxTokens, ReserveOutput}`：

- **输入预算** = `MaxTokens - ReserveOutput`（不为负）。输出预留优先，
  调用方的输入偏好永远不得挤占输出空间。
- **层级**：`BudgetPolicy{Global, Session, Request}` 逐字段解析，
  非零字段按 **Request > Session > Global** 取舍，层级冲突的结果确定且可复现。
- 输入预算 ≤ 0 时返回 `ErrBudgetTooSmall` 与空的降级结果，不会返回超预算内容。
- 超预算时：为摘要预留输入预算的 1/4，其余按
  **优先级降序 → 时效降序 → ID 升序** 贪心保留；被挤出的较早内容压缩为摘要。

## 片段声明

| 字段 | 含义 |
| --- | --- |
| `ID` | 片段标识，同 ID 去重（高版本优先，同版本请求侧优先） |
| `Source` | 来源标识；同来源只保留最高版本，新版本替换旧版本 |
| `Priority` | 越大越重要，超预算时后淘汰 |
| `Version` | 来源内的版本号 |
| `CreatedAt` | 时效信息，影响保留与摘要切分 |
| `Tokens` | 预计算 token 数，0 表示按确定性规则本地估算 |
| `Unavailable` | 来源不可用标记，触发可区分的降级 |
| `ConflictKey` | 非空时，同键而内容不同的片段视为矛盾 |

矛盾取舍规则确定：**优先级高者胜 → 时效新者胜 → 来源与 ID 字典序小者胜**。
所有取舍都记录在 `Result.Decisions` 中（`included / evicted / summarized /
replaced / conflict-lost / source-unavailable`），可逐条解释。

## 长会话压缩

- 超预算时对被挤出的较早内容生成摘要，摘要以 `[早前内容摘要]` 前缀与保留原文衔接。
- 摘要通过 `Summarizer` 接口完成，默认实现 `ExtractiveSummarizer` 为本地按句抽取；
  可用 `WithSummarizer` 替换为任意本地实现。
- 摘要提交采用版本 CAS：仅当会话版本未变时，摘要安装与被覆盖片段的移除**原子生效**；
  生成失败、超时或取消时不提交，会话不留半更新状态。
- 并发请求共享会话时，每次组装都基于 `Snapshot` 的某个完整版本，
  不会出现片段与摘要交叉的混合状态。

## 缓存与失效

- `NewCache(capacity)` 启用 LRU 结果缓存，`capacity <= 0` 禁用。
- 缓存键包含 **会话版本 + 摘要版本 + 预算 + 请求素材哈希**：
  来源内容变更或摘要推进后，旧 key 自然失效，不会再被命中。
- 摘要提交成功时额外按会话整体失效，淘汰只移除整条不可变条目，不破坏一致性。
- 降级结果不缓存，避免临时故障被固化。

## 降级与错误

| 场景 | 表现 |
| --- | --- |
| 预算极端压缩 | 返回 `ErrBudgetTooSmall`，`Result.Degraded=true`，内容为空 |
| 来源不可用 | `Degraded=true`，原因含 `ErrSourceUnavailable`，其余片段正常组装 |
| 摘要失败 | `Degraded=true`，原因含 `ErrSummarizeFailed`，回退为按预算截断 |
| 摘要超时/取消 | `Degraded=true`，原因含 `ErrSummarizeTimeout`，回退为按预算截断 |

任何情况下 `Result.Content` 的估算 token 数都不超过输入预算。

## 测试

```sh
go test -race ./...
```

覆盖并发读写与摘要推进、超预算压缩、摘要失败/超时、来源更新、
缓存命中/失效/淘汰等场景。
