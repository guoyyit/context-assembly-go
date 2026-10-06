# context-assembly-go

在请求处理链路上提供**可配置、可复现**的上下文组装能力。调用方按请求声明输入片段，
组装器按统一规则计算占用、在预算内取舍，并对长会话做本地摘要压缩；
所有取舍原因都可从组装结果中追溯。

## 核心概念

| 概念 | 类型 | 说明 |
| --- | --- | --- |
| 输入片段 | `Fragment` | 携带来源 `Source`、版本 `Version`、优先级 `Priority`、时效 `Timestamp`、互斥组 `ConflictGroup`、`Pinned`（关键约束）与 `Unavailable`（来源不可用）标记 |
| 预算 | `Budget` | `MaxTokens`（窗口总容量）与 `ReserveOutput`（输出预留） |
| 摘要组件 | `Summarizer` | 可替换的本地接口，默认实现 `LocalSummarizer` 为确定性抽取式摘要，不依赖外部服务 |
| 会话 | `Session` / `Store` | 保存片段与摘要状态，版本化，支持快照读与 CAS 摘要提交 |
| 组装器 | `Assembler` | 执行去重、冲突取舍、预算分配、摘要触发、降级与缓存 |
| 结果 | `Result` | 入选片段、摘要、每条排除记录（含原因）、生效预算、降级信息 |

## 预算语义

输入预算：`InputLimit = MaxTokens - ReserveOutput`。**任何返回结果的实际占用
（`TokensUsed`，含摘要）都不超过输入预算。**

预算支持三层设置：默认层（`Assembler.DefaultBudget`）< 会话层（`Request.SessionBudget`）< 请求层（`Request.Budget`）。
层级冲突时按确定性规则合并（`BudgetLayers.Resolve`）：

- `MaxTokens` 取各层**最小值**（最严格的容量上限生效）；
- `ReserveOutput` 取各层**最大值**（最严格的输出保护生效）。

因此请求级偏好可以进一步收紧预算，但**永远不能挤占为输出预留的空间**。
各层中 `<= 0` 的字段视为未设置。

## 组装规则

1. **占用口径统一**：所有片段与摘要都经 `TokenCounter` 计量；片段可用 `Tokens`
   字段预计算，否则由 `TokenCounter` 估算。默认 `DefaultTokenCounter`：
   CJK 字符 1 token，其余约 4 字符 1 token。
2. **去重**：同一 `(Source, ID)` 视为同一内容的不同版本，写入会话时即只保留最高
   `Version`；过期版本写入被忽略，相同内容重复写入是幂等的（不推进版本，缓存不失效）。
   被替换的旧版本会记录在结果的 `Excluded`（原因 `superseded`）中。
3. **来源冲突**：`ConflictGroup` 非空的片段在组内互斥，按
   **优先级降序 → 时间降序 → 来源升序 → ID 升序** 的确定性规则取舍，
   落选者记录为 `conflict-lost`。
4. **预算内取舍**：全部放得下则全部保留；放不下时优先对**最旧的连续前缀**生成摘要
   （保证摘要与保留原文语义衔接），旧摘要文本会折叠进新摘要。摘要目标长度为
   `输入预算 × SummaryTargetRatio`。不做摘要时按
   **Pinned 优先 → 优先级降序 → 时间降序 → ID 升序** 贪心装入。
5. **输出顺序**：入选片段按时间升序（并列按 ID）输出，结果可复现。

## 并发模型

- 读方通过 `Session.Snapshot()` 获取某一**完整版本**的不可变视图；
- 写方（`Upsert`、`CommitSummary`）在写锁内原子完成并推进版本号；
- 摘要生成在锁外进行，提交时按基版本 CAS：期间若有并发写入则提交失败、
  基于新快照重试；**生成失败或取消不会留下半更新状态**；
- 因此摘要推进与片段更新并发发生时，任何请求读到的上下文都对应某个完整版本，
  不会出现片段与摘要交叉的混合状态。

## 缓存语义

- 缓存键为 `(会话, 版本, 生效预算)`：来源内容变更或摘要推进都会推进版本，
  旧结果**自然失效**；预算不同也不会误命中；
- 容量受限，LRU 淘汰只删除整条结果，不破坏一致性；
- 降级结果（`Degraded != nil`）不写入缓存，恢复后重试会重新计算；
- `NewCache(0)` 或 `Cache = nil` 表示关闭缓存。

## 降级与错误

异常链路返回**带 `Degradation` 的降级结果**与**可区分的错误**，且不超预算：

| 场景 | 错误 | `Degradation.Kind` |
| --- | --- | --- |
| 关键（Pinned）来源不可用 | `ErrSourceUnavailable` | `source-unavailable` |
| 摘要生成超时 | `ErrSummaryTimeout` | `summary-timeout` |
| 摘要生成失败 | `ErrSummaryFailed` | `summary-failed` |
| 预算极端压缩 / Pinned 装不下 | `ErrBudgetTooSmall` | `budget-compressed` |

关键约束（`Pinned`）装不下或来源不可用时**不会静默丢弃**：必有排除记录与降级标记。
请求 `context` 被取消时直接返回 `ctx.Err()`，不改变会话状态。

## 配置项（`Assembler`）

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `Store` | 新建 | 会话存储 |
| `Cache` | `NewCache(128)` | 结果缓存，`nil` 关闭 |
| `Counter` | `DefaultTokenCounter{}` | token 计量规则 |
| `Summarizer` | `LocalSummarizer{}` | 本地摘要组件，`nil` 关闭长会话压缩 |
| `DefaultBudget` | `{8192, 1024}` | 默认预算层 |
| `SummaryTimeout` | 5s | 单次摘要生成超时 |
| `SummaryTargetRatio` | 0.25 | 摘要目标长度占输入预算的比例 |

## 使用示例

```go
store := assembly.NewStore()
a := assembly.NewAssembler(store) // 默认配置，可按需覆盖字段

res, err := a.Assemble(ctx, assembly.Request{
    SessionID: "conv-1",
    Fragments: []assembly.Fragment{
        {ID: "sys", Source: "system", Version: 1, Priority: 100, Pinned: true, Content: "你是助手"},
        {ID: "m1", Source: "chat", Version: 1, Priority: 1, Timestamp: t1, Content: "..."},
    },
    Budget: &assembly.Budget{MaxTokens: 4096, ReserveOutput: 1024},
})
if err != nil {
    // errors.Is(err, assembly.ErrSummaryTimeout) 等可区分降级原因
}
_ = res.Summary    // 摘要文本（如有）
_ = res.Fragments  // 入选片段（时间升序）
_ = res.Excluded   // 每条排除记录及原因
```

## 测试

```sh
go test ./...        # 全部测试
go test -race ./...  # 含并发场景的竞态检测
```

覆盖：并发共享会话、超预算取舍、摘要失败/超时/取消、来源更新与缓存失效、
冲突取舍、极端预算压缩等场景。
