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

## 2026-04-28 v6.9.41 review follow-up

### Observations

- 本轮 `v6.9.41` selective port 只跟了 OpenAI Images handler 的 unsupported model 早拒绝。
- review 指出：`sdk/api/handlers/openai/openai_images_handlers.go::imagesEditsFromMultipart` 先用 `c.PostForm("model")` 读取模型，再调用 `decodeImagesEditsMultipartRequest(...)`。
- 当前 body 大小限制安装点在 `sdk/api/handlers/openai/openai_images_payload.go::decodeImagesEditsMultipartRequest`，即 `http.MaxBytesReader(...)`。
- Gin 的 `PostForm` 会触发表单解析，因此如果先 `PostForm`、后 `MaxBytesReader`，multipart 解析会在限流安装前发生。

### Hypotheses

#### H1：multipart 路径先 `PostForm` 后装 `MaxBytesReader`，导致超大 body 可以绕过原有限制
- Supports：review 指向的代码路径成立；Gin `PostForm` 会解析 multipart 表单。
- Conflicts：还没用本地测试验证超大 body 是否真的会先走 unsupported model 早拒绝。
- Test：把 multipart model 前置读取改成“先安装 body limit、再统一解析 form”，并补超大 body 回归测试。

#### H2：只要在 handler 里更早调用 `http.MaxBytesReader`，继续保留 `c.PostForm("model")` 就足够
- Supports：如果 limit 在第一次解析前就装好，理论上不会再绕过。
- Conflicts：`PostForm` 会吞解析错误；一旦第一次解析因为 body 太大失败，后续 decode 可能只看到已消费的请求体，错误形态不稳定。
- Test：避免 `PostForm`，统一复用共享 multipart helper 返回 `*multipart.Form` 和错误。

#### H3：review 只是测试空白，不是实际 bug
- Supports：当前新增测试都通过。
- Conflicts：代码路径上，限流确实发生在 `PostForm` 之后，属于真实顺序回归。
- Test：补一条“unsupported model + 超大 multipart body”测试，看当前是否还能稳定报 body-too-large。

### Experiments

#### E1：核对 Gin `PostForm` 与标准库 `ParseMultipartForm` 行为
- Change：无代码变更，只读查看 Gin 与 `net/http` 源码。
- Expected：如果 `PostForm` 会在 `PostFormValue` / `ParseMultipartForm` 链路里主动解析 multipart，那么 review 根因成立。
- Result：Confirmed。Gin `Context.initFormCache()` 会调用 `req.ParseMultipartForm(...)`；标准库 `PostFormValue` 也会在必要时触发 `ParseMultipartForm(...)`。

#### E2：改成共享 `imagesMultipartFormWithLimit(...)`，让 handler 与 decode 复用同一次受限解析
- Change：新增 `sdk/api/handlers/openai/openai_images_payload.go::imagesMultipartFormWithLimit` 与 `firstMultipartFormValue`，`imagesEditsFromMultipart(...)` 不再调用 `c.PostForm("model")`，而是先通过共享 helper 安装 limit 并拿到已解析 form。
- Expected：限流会在首次 multipart 解析前生效；超大 body 不会再因为 unsupported model 早拒绝逻辑而绕过 size limit。
- Result：Confirmed。endpoint 级用例与包级测试通过，且新增超大 body 测试不再落到 unsupported model 错误。

### Root Cause

- 本轮回归的根因是 multipart 路径为了前移 unsupported model 校验，过早调用了 `c.PostForm("model")`，从而让表单在 `http.MaxBytesReader` 安装前就被解析。

### Fix

- 新增 `sdk/api/handlers/openai/openai_images_payload.go::imagesMultipartFormWithLimit(...)`，统一负责：
  - 在第一次 multipart 解析前安装 `MaxBytesReader`
  - 解析并缓存 `*multipart.Form`
- `sdk/api/handlers/openai/openai_images_handlers.go::imagesEditsFromMultipart` 现在先通过共享 helper 取 `form`，再用 `firstMultipartFormValue(form, "model")` 做 unsupported model 早拒绝，不再直接调用 `c.PostForm(...)`。
- `sdk/api/handlers/openai/openai_images_validation_test.go` 新增 `TestImagesEditsMultipart_UnsupportedModelDoesNotBypassBodyLimit`，锁住“大 body 仍优先命中 size limit，而不是 unsupported model shortcut”的语义。

## 2026-04-30 disable-image-generation review follow-up

### Observations

- `internal/runtime/executor/payload_helpers.go::removeToolTypeFromToolsArray` 删除 `image_generation` 后原先会写回 `tools: []`；当原数组只有图片工具时会留下空数组。
- `internal/runtime/executor/iflow_executor.go::ExecuteStream` 已在 `applyPayloadConfigWithRoot(...)` 之前规避空 `tools` 数组，因此 payload helper 后续重新写出空数组会绕过这层兼容处理。
- `internal/api/modules/amp/fallback_handlers.go::WrapHandler` 会在没有本地 provider 时直接 `proxy.ServeHTTP(...)`，并且发生在 OpenAI Images handler 的 `rejectDisabledImageGeneration(...)` 之前。
- `internal/api/modules/amp/routes.go::registerProviderAliases` 把 `/api/provider/:provider[/v1]/images/*` 包在 `FallbackHandler.WrapHandler(...)` 里。
- `internal/translator/codex/openai/responses/codex_openai-responses_request.go` 已支持 `tool_choice.tools` 形状；此前 `payload_helpers.go::removeToolTypeFromPayloadWithRoot` 只清理顶层 `tools`。

### Hypotheses

#### H1: payload helper 只写回空数组导致上游不兼容（ROOT）
- Supports: review 指向 `removeToolTypeFromToolsArray`，iFlow 兼容逻辑在 payload helper 之前。
- Conflicts: 混合工具数组不受影响。
- Test: 新增只含 `image_generation` 的 payload helper 用例，期望 `tools` 字段被删除。

#### H2: AMP fallback 没有读全局禁用开关导致绕过 404（ROOT）
- Supports: `WrapHandler` 在无 provider 时直接代理，OpenAI Images handler 不会执行。
- Conflicts: 有本地 provider 时仍会进入 handler，被现有 404 覆盖。
- Test: 构造 `/api/provider/openai/v1/images/generations` fallback 请求，禁用开关开启时确认不调用 proxy 和 wrapped handler。

#### H3: Responses allowed_tools 的 `tool_choice.tools` 未清理导致禁用语义不完整（ROOT）
- Supports: translator 支持 `tool_choice.tools`，payload helper 只清理 `tools`。
- Conflicts: 顶层 `tool_choice.type=image_generation` 按既定方案保持不改。
- Test: 构造 `tool_choice.type=allowed_tools` 且 tools 内含 `image_generation`，期望清理该引用。

### Root Cause

- 全局图片禁用逻辑只覆盖了主 OpenAI Images handler 和顶层 `tools` 数组，没有覆盖 AMP fallback 的代理前分支、Responses `tool_choice.tools` 形状，以及删除最后一个工具后的空数组兼容问题。

### Fix

- `payload_helpers.go::removeToolTypeFromPayloadWithRoot` 同时清理顶层 `tools` 与 `tool_choice.tools`，并在最后一个工具被移除时删除对应 tools 字段。
- `payload_helpers.go::removeEmptyAllowedToolsChoiceWithRoot` 只在本轮确实移除了 `tool_choice.tools` 里的图片工具后，删除已经没有 allowed tools 的 `tool_choice`。
- `fallback_handlers.go::WrapHandler` 在 AMP provider alias 图片路径上先检查全局禁用开关，命中时直接返回 404，不进入 wrapped handler 或 proxy。
- `routes.go::registerProviderAliases` 将 `BaseAPIHandler.Cfg.DisableImageGeneration` 绑定给 fallback handler，支持热更新后的配置读取。
- 新增 payload helper、fallback handler 和 provider alias 回归测试。
