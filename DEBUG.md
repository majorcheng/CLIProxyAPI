# 调试记录

## Observations

- 用户反馈：management 接口里的 usage 统计中，每次 `gpt-5.2` 访问后都会多一条 `gpt-image-2` 访问，但客户端实际上没有调用图片模型。
- 当前怀疑范围集中在最近 `v6.9.39` selective port 引入的 Codex 图片工具 usage 发布链，以及 management usage 展示链。
- 本轮先做只读排查，不先改业务代码。

## Hypotheses

### H1：management 展示层把普通 Codex 请求误映射成了 `gpt-image-2`
- Supports：用户看到的是 management `/usage` 结果，而不是原始上游响应。
- Conflicts：`internal/api/handlers/management/usage.go::GetUsageStatistics` 直接返回内存 snapshot，没有做模型名二次推断。
- Test：检查 management usage handler 是否直接透传 snapshot。

### H2：Codex 图片工具 usage 发布链把“`response.tool_usage.image_gen` 对象存在”直接当成真实图片模型请求
- Supports：`parseCodexImageToolUsage` 只校验对象存在；`publishAdditionalModel` 会无条件追加一条模型记录；usage 统计即使 token 为 0 也会增加请求数。
- Conflicts：还没有现场抓到一份真实 text-only Codex completed payload，确认其中 `image_gen` 对象到底长什么样。
- Test：沿 `parseCodexImageToolUsage -> publishCodexImageToolUsage -> publishAdditionalModel -> logger_plugin.updateAPIStats` 静态核对；再检查没有 image tool 的普通文本请求是否会回退成默认 `gpt-image-2`。

### H3：OpenAI Images / 路由前缀逻辑把普通 `gpt-5.2` 请求错误地改写成了 `gpt-image-2`
- Supports：最近同一批改动里也动了 images 路由和 `gpt-image-2` builtin。
- Conflicts：普通文本请求在 `prepareCodexRequestPlan` 下不会自动注入 image tool；images 入口与普通 Responses 路径是分开的。
- Test：检查普通文本请求的 request plan 是否仍然没有 `image_generation` tool。

## Experiments

### E1：核对 management usage 是否只是透传 snapshot
- Change：无代码变更，只读检查 `internal/api/handlers/management/usage.go::GetUsageStatistics`。
- Expected：如果 handler 直接返回 snapshot，则额外的 `gpt-image-2` 一定来自后端 usage 发布链，而不是 management 展示层。
- Result：Confirmed。`GetUsageStatistics` 直接把 `h.usageStats.Snapshot()` 原样返回。

### E2：核对 Codex 图片工具 usage 发布条件
- Change：无代码变更，只读检查 `internal/runtime/executor/usage_helpers.go` 与 `internal/runtime/executor/codex_executor.go`。
- Expected：如果 `parseCodexImageToolUsage` 仅按 `response.tool_usage.image_gen` 对象存在返回 `ok=true`，且 `publishAdditionalModel` 不校验 token/num_images/是否真的有 image tool，那么只要上游回包带了该对象，就会额外生成一条模型 usage。
- Result：Confirmed。`parseCodexImageToolUsage` 仅要求对象存在；`publishCodexImageToolUsage` 随后直接调用 `reporter.publishAdditionalModel(...)`；`logger_plugin.updateAPIStats` 对 0 token 记录同样递增 `TotalRequests`。

### E3：核对普通文本请求是否仍然没有 image tool
- Change：无代码变更，只读检查 `internal/runtime/executor/codex_request_plan_imagegen_test.go::TestCodexPrepareRequestPlan_DoesNotInjectImageGenerationToolForPlainTextRequest`。
- Expected：如果普通文本请求 body 本身没有 `image_generation` tool，那么额外 usage 模型名只能来自 `codexImageGenerationToolModel` 的默认回退值。
- Result：Confirmed。plain text request plan 不注入 image tool；而 `codexImageGenerationToolModel` 在找不到 image tool 时会回退到默认 `gpt-image-2`。

## Root Cause

- 当前额外的 `gpt-image-2` usage 不是 management 展示层误判，而是 Codex usage 发布链的回归：只要 completed payload 里出现 `response.tool_usage.image_gen` 对象，就会额外发布一条模型 usage；若请求体本身没有 image tool，还会默认把这条记录记成 `gpt-image-2`。

## Fix

- `internal/runtime/executor/codex_executor.go::publishCodexImageToolUsage` 现已改成双门闩：
  - 请求体必须显式表达 `image_generation` 意图
  - completed 响应里必须真实出现 `image_generation_call`
- `internal/runtime/executor/usage_helpers.go` 新增 `codexResponseUsedImageGenerationTool(...)`，单独存在 `response.tool_usage.image_gen` 不再视为真实图片调用。
- `internal/runtime/executor/codex_executor.go::ExecuteStream` 现在会先对 `response.completed` 做 output patch，再发布图片 additional usage，避免真实图片调用只出现在 `response.output_item.done` 时被漏记。
- 新增回归测试覆盖：
  - 普通文本请求带 `tool_usage.image_gen` 元数据时不再误发 `gpt-image-2`
  - 真实图片调用即使没有图片 token 明细，仍会发布 additional image model usage
  - 流式 completed 缺少 `response.usage`、且这次不是图片调用时，仍会保留主模型请求计数

## Review Follow-up

- reviewer 指出：流式 `response.completed` 若没有 `response.usage`，且因为不是图片调用而跳过 `publishCodexImageToolUsage(...)`，则主模型计数会被一起丢掉。
- 复核后确认根因成立：`ExecuteStream` 的 `response.completed` 分支没有像非流式 `readCodexCompletedEvent(...)` 那样在末尾无条件执行 `reporter.ensurePublished(ctx)`。
- 修复：在 `internal/runtime/executor/codex_executor.go::ExecuteStream` 的 `response.completed` 分支末尾补回 `reporter.ensurePublished(ctx)`，保持：
  - 主模型成功请求计数总能保留
  - additional image usage 仍只在真实图片调用时发布
