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

## 2026-05-01 fill-first 删除后仍命中候选确认

### Observations

- 用户反馈：401 已经被删除的 token 没有从 fill-first 候选列表里移除，后续仍可能被请求并失败。
- 当前 HEAD 已拉到 `origin/main` 最新，`git pull --ff-only` 返回 `Already up to date.`。
- `sdk/cliproxy/auth/fill_first_selection.go::scanFillFirstCandidates` 本身只扫描传入 auth 切片；是否还会选到已删 token，取决于上游 manager/scheduler 是否同步禁用或移除对应 auth。
- `sdk/cliproxy/service.go::deleteAuthMaintenanceCandidate` 的 401 自动维护删除路径会对 candidate 内所有 ID 发 delete update；既有测试覆盖“后台维护删除后不再请求坏 token”。
- 管理端 `internal/api/handlers/management/auth_files.go::deleteAuthFileByName` 先用 `findAuthForDelete` 找单个 auth，再调用 `disableAuth` 禁用单个 ID；如果同一 token 文件派生多个 auth ID，兄弟 ID 会继续保持 active。

### Hypotheses

#### H1：401 自动维护删除没有刷新 scheduler，导致 fill-first 继续选到已删除 auth
- Supports：用户现象直接指向 fill-first 候选未删。
- Conflicts：`applyCoreAuthRemovalWithReason -> coreManager.Update -> scheduler.upsertAuth(disabled)` 会触发 `removeAuthLocked`；既有后台维护压力测试也覆盖删除后不再请求坏 auth。
- Test：跑现有 scheduler/maintenance 删除测试，并写临时 fill-first 诊断测试验证 401 维护删除后的下一次请求。

#### H2：管理端/单文件删除只禁用一个 auth ID，同源兄弟 ID 留在 fill-first 候选里（ROOT）
- Supports：`findAuthForDelete` 只返回一个 auth；`disableAuth` 只禁用一个 ID；同一 Codex token 文件可派生 primary/project 等多个 auth ID。
- Conflicts：维护删除路径有 `authMaintenanceIDsForPath`，不走这个单 ID 删除边界。
- Test：临时构造两个 auth 共用一个 token 文件，管理端删除该文件后立刻执行 fill-first 请求，观察剩余兄弟 ID 是否仍被调用。

#### H3：session affinity 缓存绑定了已删 auth，绕过 fill-first 正常候选过滤
- Supports：sticky 包装层有独立缓存。
- Conflicts：`collectAffinityCandidates` 会过滤 disabled 和 registry 不支持模型的 auth；401/429 shared block 也会触发 affinity invalidation。
- Test：静态核对 `collectAffinityCandidates` 与 `SessionAffinitySelector.pickBoundAuth` 的可用性过滤。

### Experiments

#### E1：跑现有删除与 scheduler 定向测试
- Change：无生产代码变更。
- Command：`timeout 180s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'TestManager_SchedulerTracksRegisterAndUpdate|TestDeleteAuthMaintenanceCandidate_RemovesFileAndDisablesAllAuths|TestAuthMaintenanceBackgroundQueue_MixedLoadGraduallyRemoves401And429' -count=1`
- Result：Confirmed pass。`sdk/cliproxy/auth` 与 `sdk/cliproxy` 均通过，说明现有覆盖下“disabled/update 后 scheduler 移除”和“维护后台删除后不再请求坏 auth”成立。

#### E2：临时验证 fill-first + 401 维护删除主链
- Change：临时新增 `sdk/cliproxy/service_auth_maintenance_fill_first_diag_test.go`，跑完自动删除。
- Expected：首轮 fill-first 命中 `bad-old` 401 后可切到 `good-new`，后续请求不再调用 `bad-old`。
- Result：Confirmed pass。最终调用计数保持 `bad-old=1`，后续只增加 `good-new`。

#### E3：临时验证管理端删除同源多 auth 文件
- Change：临时新增 `internal/api/handlers/management/auth_files_delete_fillfirst_diag_test.go`，跑完自动删除。
- Expected：如果管理端只禁用一个 ID，则删除文件后仍会有一个同源兄弟 auth active，并被 fill-first 选中。
- Result：Confirmed。诊断测试通过：删除同一个 `shared-token.json` 后，两个同源 auth 中只剩一个被禁用，另一个仍 active；随后 fill-first 请求命中了这个剩余 active 兄弟 ID。

### Root Cause

- 当前确认的根因不是 `scanFillFirstCandidates` 自身漏删，而是管理端/单文件删除路径只按 `findAuthForDelete` 命中的单个 auth ID 调用 `disableAuth`；当同一 token 文件派生多个 auth ID 时，兄弟 ID 没有同步禁用、没有从 scheduler/fill-first 候选中移除。

### Fix Direction

- 修复入口应收口在管理端删除路径：按实际 backing path 找出同源所有 auth ID，并统一禁用、注销 registry、刷新 scheduler，而不是只禁用 `FindByFileName` 返回的第一个 auth。
- 自动维护删除路径已有 `authMaintenanceIDsForPath` 这类按 path 聚合的逻辑，可复用或抽出共享 helper，避免管理端和维护端删除语义分叉。

### Fix Implemented

- `internal/api/handlers/management/auth_files.go::deleteAuthFileByName` 不再只禁用 `findAuthForDelete(...)` 返回的单个 ID，而是调用 `disableAuthsForDeletedPath(...)` 按实际 backing path 禁用同源 auth。
- 新增 `internal/api/handlers/management/auth_files_delete_scope.go`，集中实现 path 归一化、同源 auth ID 枚举和去重禁用。
- `disableAuth(...)` 在管理端删除语义下同步 `registry.GetGlobalRegistry().UnregisterClient(auth.ID)`，避免已删除 token 继续留在模型 registry 候选面。
- 新增 `internal/api/handlers/management/auth_files_delete_fill_first_test.go::TestDeleteAuthFile_DisablesAllAuthsForSharedBackingPath`，覆盖同一 token 文件派生多个 auth ID 时，删除后 fill-first 只能命中健康 token，不能再请求已删 token 的兄弟 auth。
- 验证命令：
  - `timeout 180s go test ./internal/api/handlers/management -count=1`
  - `timeout 180s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'TestManager_SchedulerTracksRegisterAndUpdate|TestDeleteAuthMaintenanceCandidate_RemovesFileAndDisablesAllAuths|TestAuthMaintenanceBackgroundQueue_MixedLoadGraduallyRemoves401And429' -count=1`

### Review Follow-up: management 删除同源 auth 的剩余闭环

#### Observations
- `DELETE /v0/management/auth-files?name=...` 已改为 `disableAuthsForDeletedPath(...)`，但 `all=true` 分支仍在逐文件归档后调用 `disableAuth(ctx, full)`，只能命中路径派生 ID，无法清理同一文件派生出的自定义兄弟 auth ID。
- 新增的 `normalizeAuthDeletePath(...)` 只做 `Abs/Clean`，没有复用 `authIDForPath(...)` 的 Windows 小写归一化；大小写不敏感文件系统上同一路径可能因大小写差异被拆成两个分组。

#### Hypotheses
- H1: `all=true` 分支仍使用旧禁用入口导致批量删除漏禁同源 auth。Supports: 源码仍为 `disableAuth(ctx, full)`；Conflicts: 按名删除路径已经修复。Test: 构造 shared primary/project 后调用 `?all=true`，断言两个 shared auth 都 disabled 且 registry 清空。
- H2: Windows 路径归一化缺少大小写折叠导致同源 auth 分组分裂。Supports: `authIDForPath` 已有 `runtime.GOOS == "windows"` 小写逻辑，新增 helper 没有；Conflicts: Linux 本机大小写敏感，无法直接复现真实 Windows FS。Test: 把大小写折叠抽成可测 helper，用 Windows 风格路径验证大小写差异归一到同一个 key。
- H3: registry 残留而非 manager disabled 是唯一问题。Supports: registry 会影响模型候选可见性；Conflicts: `all=true` 下 manager 本身也不会禁用自定义兄弟 auth。Test: 同时断言 manager disabled 和 registry cleared。

#### Root Cause
批量删除和路径归一化没有复用按名删除的新语义：`all=true` 仍只禁用单个路径 ID，且删除路径匹配 key 未按大小写不敏感平台折叠。

#### Fix Plan
- `all=true` 分支改为调用 `disableAuthsForDeletedPath(ctx, full, "")`。
- 删除路径归一化补上与 `authIDForPath` 一致的平台折叠，并拆成可测的 path key helper。
- 增加 `all=true` 同源 auth 回归测试和 Windows 风格路径大小写归一化测试。

#### Experiments
- 新增 `TestDeleteAuthFileAll_DisablesAllAuthsForSharedBackingPath`：批量删除 shared/good token 后，断言 shared primary/project/good 全部 disabled、registry 清空，且后续 fill-first 不会调用任何已删除 auth。
- 新增 `TestNormalizeAuthDeletePathForCase_CaseInsensitiveKey`：大小写不敏感 key 下，同一路径的大小写差异归一到同一个删除匹配 key。

#### Fix Implemented
- `DeleteAuthFile` 的 `all=true` 分支从 `disableAuth(ctx, full)` 改为 `disableAuthsForDeletedPath(ctx, full, "")`，与按名删除复用同源 auth 禁用语义。
- `normalizeAuthDeletePath(...)` 改为通过 `normalizeAuthDeletePathForCase(...)` 生成匹配 key，并在 Windows 平台按现有 `authIDForPath(...)` 语义折叠为小写。
- 验证通过：`timeout 180s go test ./internal/api/handlers/management -count=1`、`timeout 180s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'TestManager_SchedulerTracksRegisterAndUpdate|TestDeleteAuthMaintenanceCandidate_RemovesFileAndDisablesAllAuths|TestAuthMaintenanceBackgroundQueue_MixedLoadGraduallyRemoves401And429' -count=1`、`git diff --check`。
- 复核补充：相对路径且无配置快照时也通过 `foldAuthDeletePathCase(...)` 折叠大小写，避免早返回绕过大小写不敏感 key 规则。

### Review Follow-up: v6.10.0 统计口径窗口错位

#### Observations
- 当前 `sdk/cliproxy/auth/types.go::Success/Failed` 是进程内累计总数，`sdk/cliproxy/auth/types.go::RecentRequestsSnapshot` 只返回最近 20 个 10 分钟桶。
- `internal/api/handlers/management/api_key_usage.go::addAPIKeyUsageAuth` 之前直接把 `auth.Success/auth.Failed` 和 `recent_requests` 并排返回；`internal/api/handlers/management/auth_files.go::buildAuthFileEntry` 也直接返回累计总数。
- 这会导致 management API 同一响应对象里同时出现“累计总数”和“窗口趋势”，服务运行足够久后两者必然不一致。

#### Hypotheses
- H1：应该把 `Auth.Success/Failed` 本身改成窗口总数。Supports：可以和 `recent_requests` 自然对齐。Conflicts：字段已在 `MarkResult` 里定义为累计统计，改内部语义会扩大影响面。 Test：检查是否存在只依赖累计字段而不看窗口的内部调用。
- H2：应该保留 `Auth.Success/Failed` 的累计语义，只修 management 对外暴露，让 `success/failed` 从 `recent_requests` 汇总得出。（ROOT）Supports：review 指向的是同一响应对象口径错位；当前没有其他仓内消费者读取 `/api-key-usage`。 Conflicts：上游新增字段原意更接近累计展示。 Test：把 auth 的累计值手工改成 42/24，验证 management 仍返回窗口 1/1。
- H3：应该删除 management 响应里的 `success/failed` 字段，仅保留 `recent_requests`。Supports：最彻底避免歧义。 Conflicts：会偏离本轮要跟的上游小增强。 Test：比对本轮 selective port 范围，确认这会丢功能而不是修语义。

#### Experiments
- E1：静态检索 `internal/api/handlers/management`、`internal/tui`、`sdk/api`，确认仓内没有其他消费者依赖 `/v0/management/api-key-usage` 的旧数组值或累计总数字段。结果：Confirmed，当前仅有路由、handler 与测试，没有前端/TUI 直连此接口。
- E2：在 `api_key_usage` 和 `auth_files` 测试里把同一 auth 的 `Success/Failed` 人工改成 `42/24`，同时保留 `recent_requests` 为 `1/1`。结果：Confirmed，旧实现会把 42/24 直接透出，证明 review 指出的错位真实存在。

#### Root Cause
- 根因不是 `Success/Failed` 字段本身有问题，而是 management 端把“累计总数”直接和“最近 20 个 10 分钟桶”放进同一响应对象，导致对外统计口径错位。

#### Fix Implemented
- `internal/api/handlers/management/api_key_usage.go` 新增 `recentRequestBucketTotals(...)`，`success/failed` 改为由 `RecentRequestsSnapshot(...)` 的 20 桶窗口汇总得出，不再直接暴露 `auth.Success/auth.Failed`。
- `internal/api/handlers/management/auth_files.go::buildAuthFileEntry` 同样改成先取 `recentRequests := auth.RecentRequestsSnapshot(time.Now())`，再从窗口桶汇总 `success/failed`，确保同一响应对象口径一致。
- `sdk/cliproxy/auth/types.go` 注释补充说明：`Success/Failed` 是进程内累计计数，若 management 要和 `recent_requests` 同窗口展示，必须自行按时间桶汇总。
- 新增回归测试：
  - `internal/api/handlers/management/api_key_usage_test.go::TestGetAPIKeyUsage_UsesRecentWindowTotalsInsteadOfCumulativeAuthTotals`
  - `internal/api/handlers/management/auth_files_recent_requests_test.go::TestListAuthFiles_UsesRecentWindowTotalsInsteadOfCumulativeAuthTotals`
- 验证通过：
  - `timeout 60s go test ./internal/api/handlers/management -run 'Test(GetAPIKeyUsage|ListAuthFiles).*' -count=1`
  - `timeout 60s go test ./sdk/cliproxy/auth -run 'Test.*RecentRequests|TestManager.*RecentRequests|TestManagerUpdatePreservesRecentRequests' -count=1`
  - `timeout 60s go test ./internal/api/handlers/management ./sdk/cliproxy/auth -count=1`
  - `timeout 60s go test ./... -run '^$'`
  - `timeout 60s git diff --check`

### Review Follow-up: Claude Responses tool_result 非字符串回归

#### Observations
- `internal/translator/claude/openai/responses/claude_openai-responses_request.go::convertResponsesToolResultContent` 当前会把数组里的可识别 part 转成 Claude content blocks，但未识别 part 会被直接跳过。
- 同文件 `convertResponsesContentPartToClaudePart` 在处理 `type=file` 时，直接把 `file.file_data` 写进 `document.source.data`，没有剥离 `data:...;base64,` 前缀。
- 当前新增测试只覆盖 `text + image_url` 的 happy path，没有覆盖“混合数组含未知 part”以及 `file_data` 为 data URL 的场景。

#### Hypotheses
- H1：数组 tool_result 只要命中部分可识别项，函数就会返回部分转换后的 `claudeContent`，从而静默丢掉未识别项。（ROOT）Supports：当前 `partCount > 0` 即返回转换结果；Conflicts：当数组全都不可识别时会 fallback 原始 JSON。Test：加入 `text + unknown object` 混合数组，断言第二项仍保留。
- H2：`file.file_data` 若是 data URL，当前会把完整前缀写进 Claude `source.data`，导致非法 base64。Supports：代码直接 `Set(source.data, fileData)`；Conflicts：若输入本来就是裸 base64，则不会暴露问题。Test：加入 `data:text/plain;base64,...` 用例，断言 `source.data` 只剩裸 base64。
- H3：问题来自测试断言而不是实现。Supports：无。Conflicts：静态审查已经能直接看出实现路径丢字段与未剥头。Test：补针对性测试。

#### Experiments
- E1：新增混合数组回归测试 `TestConvertOpenAIResponsesRequestToClaude_PreservesRawToolResultArrayWhenPartIsUnsupported`，使用 `text + {"foo":"bar"}` 输入验证未知项是否被保留。
- E2：新增 data URL 文件回归测试 `TestConvertOpenAIResponsesRequestToClaude_StripsFileDataURLPrefix`，验证 `document.source.data` 只保留裸 base64，`media_type` 正确提取为 `text/plain`。

#### Root Cause
- 根因是 translator 在“部分可识别”的 tool result 数组上错误地选择了部分转换结果，而不是在遇到未知 part 时整体 fallback 原始 JSON；同时 `file_data` 缺少 data URL 剥头逻辑。

#### Fix Implemented
- `convertResponsesToolResultContent(...)` 新增 `hasUnsupportedPart` 路径：数组里一旦出现未知 part，就整体 fallback 为原始 JSON，避免选择性丢字段。
- `convertResponsesContentPartToClaudePart(...)` 改为通过 `decodeResponsesToolResultFileData(...)` 解析 `file.file_data`，正确剥离 `data:...;base64,` 前缀并回填 `media_type`。
- 新增回归测试：
  - `internal/translator/claude/openai/responses/claude_openai-responses_request_test.go::TestConvertOpenAIResponsesRequestToClaude_PreservesRawToolResultArrayWhenPartIsUnsupported`
  - `internal/translator/claude/openai/responses/claude_openai-responses_request_test.go::TestConvertOpenAIResponsesRequestToClaude_StripsFileDataURLPrefix`

### Review Follow-up: v6.10.5/v6.10.6 OpenAI Compat thinking default 与 Claude refresh singleflight 上下文回归

#### Observations
- `internal/runtime/executor/openai_compat_executor.go::Execute` 与 `ExecuteStream` 先对 `translated` 执行 `thinking.ApplyThinking(...)`，随后把未经过 thinking 的 `originalTranslated` 传给 `applyPayloadConfigWithRoot(...)` 作为 default 缺字段判断源。
- `applyPayloadDefaultRulesWithRoot(...)` / `applyPayloadDefaultRawRulesWithRoot(...)` 只看 `source` 是否存在字段；因此模型后缀 `(high)` 注入到 `translated.reasoning_effort` 后，仍会被 `payload.default.reasoning_effort=low` 判定为“原始缺失”并覆盖。
- `internal/auth/claude/anthropic_auth.go::RefreshTokens` 当前用 `singleflight.Do(...)` 闭包捕获第一个调用者的 `ctx`；同一 key 后续等待者会共享第一个 HTTP 请求的取消和 deadline。
- 现有测试覆盖了 payload override 胜过 thinking suffix、Claude refresh 并发去重、deadline 返回，但缺少“payload default 不覆盖 suffix”和“第一个等待者取消不拖死后续等待者”的回归用例。

#### Hypotheses
- H1：OpenAI Compat 回归来自 payload default 的 source 没同步 thinking suffix。（ROOT）Supports：源码中 `translated` 已写入 suffix，`originalTranslated` 未写入；default helper 按 source 判缺失。Conflicts：override 规则仍应继续覆盖 suffix。Test：配置 default low，请求 `custom-openai(high)`，断言上游 body 仍是 high；同时保留 override low 测试。
- H2：OpenAI Compat 应该把 `thinking.ApplyThinking(...)` 移回 payload config 之后。Supports：旧 default 语义不会覆盖 suffix。Conflicts：这会破坏本轮要保留的“payload override 胜过 suffix”语义。Test：现有 `TestOpenAICompatExecutorPayloadOverrideWinsOverThinkingSuffix` 会约束不能回退。
- H3：Claude refresh 回归来自 singleflight 执行体继承了第一个调用者 ctx。（ROOT）Supports：`singleflight.Do` 闭包直接捕获 `ctx`；Go singleflight 只运行第一个闭包。Conflicts：无。Test：第一个调用者 10ms 超时，第二个 background 等待同一 refresh token，释放上游后第二个必须成功且只有一次上游调用。
- H4：Claude refresh 可以通过把 deadline 拼进 singleflight key 规避。Supports：不同 deadline 不再互相影响。Conflicts：手动 cancel 的 context 仍会绑架同 key 无 deadline 等待者，也会明显削弱并发去重。Test：手动取消首个 caller 时仍会失败。

#### Experiments
- E1：新增 OpenAI Compat 非流式 default/suffix 回归测试。预期旧实现得到 `reasoning_effort=low`，修复后得到 `high`。
- E2：新增 OpenAI Compat 流式 default/suffix 回归测试。预期旧实现得到 `reasoning_effort=low`，修复后得到 `high`。
- E3：新增 Claude refresh singleflight 上下文回归测试。预期旧实现中第二个等待者会收到第一个调用者的 deadline 错误；修复后第一个按自身 ctx 返回 deadline，第二个继续等待并拿到 token。

#### Root Cause
- OpenAI Compat 的根因是 suffix thinking 写在 `translated` 上，但 payload default 的缺字段判断仍读取未注入 suffix 的 `originalTranslated`。
- Claude refresh 的根因是 singleflight 执行体继承了首个调用者 ctx，把共享刷新请求生命周期绑定到了第一个等待者。

#### Fix Plan
- OpenAI Compat：对 `originalTranslated` 同步执行 `thinking.ApplyThinking(...)`，仅作为 payload default 缺字段判断源；`translated` 仍先 apply thinking 再套 payload config，保持 override 可以覆盖 suffix。
- Claude refresh：改用 `singleflight.DoChan(...)`，共享刷新执行使用内部有界 context，调用方只按自己的 `ctx` 等待结果；任一等待者取消只影响自身返回，不取消共享刷新执行。
- 补上述三个回归测试，并跑受影响包验证与 `git diff --check`。

#### Fix Implemented
- OpenAI Compat 新增 `applyOpenAICompatThinkingPayloads(...)`，同时对实际上游 payload 和 payload default 判缺 source 执行 `thinking.ApplyThinking(...)`。
- `Execute` / `ExecuteStream` 继续保持“thinking 后再套 payload config”的顺序，因此 payload override 仍能覆盖 suffix；但 default 现在会看到 suffix 注入的字段，不会把 `(high)` 改回默认 `low`。
- Claude refresh 从 `singleflight.Do(...)` 改为 `singleflight.DoChan(...)`；共享 refresh HTTP 执行使用 `claudeRefreshSingleflightTimeout` 控制的内部 context，各调用方通过自己的 `ctx` select 等待结果。
- 新增回归测试：
  - `TestOpenAICompatExecutorPayloadDefaultDoesNotOverrideThinkingSuffix`
  - `TestOpenAICompatExecutorStreamPayloadDefaultDoesNotOverrideThinkingSuffix`
  - `TestRefreshTokens_DoesNotShareFirstCallerContext`
- 验证通过：
  - `timeout 60s go test ./internal/runtime/executor -run 'TestOpenAICompatExecutor(PayloadOverrideWinsOverThinkingSuffix|PayloadDefaultDoesNotOverrideThinkingSuffix|StreamPayloadDefaultDoesNotOverrideThinkingSuffix)' -count=1`
  - `timeout 60s go test ./internal/auth/claude -run 'TestRefreshTokens_(DoesNotShareFirstCallerContext|DeduplicatesConcurrentRefresh|DoesNotDeduplicateDifferentProxyKeys)|TestRefreshTokensHonorsContextDeadline|TestRefreshTokensWithRetry_429BlocksImmediateReplay' -count=1`
  - `timeout 60s go test ./internal/runtime/executor ./internal/auth/claude -count=1`
  - `timeout 60s go test ./internal/cmd ./internal/api/handlers/management ./internal/misc ./internal/auth/claude ./internal/runtime/executor ./internal/api/modules/amp ./sdk/api/handlers/openai ./internal/translator/claude/openai/chat-completions ./internal/translator/openai/claude -count=1`
  - `git diff --check && git diff --cached --check`

### Review Follow-up: Gemini CLI 显式项目与 Responses SSE output 顺序回归

#### Observations
- `internal/misc/gemini_cli_project.go::ResolveGeminiCLIProjectID` 当前只要 `responseProjectID` 非空就返回后端项目，忽略了用户是否显式选择项目以及账号 tier。
- `internal/cmd/login.go::performGeminiCLISetup` 旧逻辑在显式项目和返回项目不一致时，只有 `gen-lang-client-*`、`FREE`、`LEGACY` 这类免费/旧账号才切到后端项目；付费账号应保留用户显式选择的项目。
- `sdk/api/handlers/openai/openai_responses_handlers.go::repairCompletedPayload` 当前先按 `output_index` 排序写入 indexed item，再把无 index item 追加到末尾；这会把实际 SSE 顺序里的 `reasoning -> message` 回填成 `message -> reasoning`。

#### Hypotheses
- H1：Gemini CLI 回归来自 helper 抽象丢失 `explicitProject` 和 `tierID` 两个决策输入。（ROOT）Supports：helper 只有两个字符串参数，无法区分付费显式项目与免费后端项目映射。Conflicts：自动发现和响应同项目场景仍可直接用 response。Test：显式付费项目 A、响应项目 B，断言最终仍是 A；显式 FREE/LEGACY 项目 A、响应项目 B，断言最终是 B。
- H2：Responses SSE 顺序回归来自把 indexed/unindexed 分别存储并在 completed 时重新排序。（ROOT）Supports：源码按 sorted indexes 写完后再追加 unindexed。Conflicts：纯 indexed 且到达顺序等于 index 顺序的用例不会暴露。Test：先发送无 index reasoning，再发送带 index message，断言 completed.output 顺序仍是 reasoning、message。
- H3：问题来自测试断言遗漏而非运行时代码。Supports：现有测试没有覆盖“无 index 在 indexed 前”的混合顺序，也没有覆盖显式付费项目。Conflicts：静态代码路径已经能直接证明运行时会重写/重排。Test：补回归测试应在旧实现上失败。

#### Fix Plan
- Gemini CLI：把项目选择 helper 改成显式输入对象，保留 `requestedProjectID`、`responseProjectID`、`explicitProject`、`tierID`，只在自动发现、响应同项目或免费/旧账号映射时采用后端项目；付费显式项目不被重写。
- Responses SSE：改为按 `response.output_item.done` 到达顺序记录完整 output item 序列，completed.output 为空时直接按记录顺序回填，不再把 indexed/unindexed 分桶后重排。
- 补两个回归测试并跑受影响包定向验证。

#### Root Cause
- Gemini CLI 的根因是项目选择 helper 在抽象时只保留 requested/response 两个项目 ID，丢掉了显式选择与 tier 这两个决定“是否允许改写项目”的关键上下文。
- Responses SSE 的根因是 completed.output 回填阶段把 indexed 与 unindexed item 分桶重排，而不是使用真实 SSE 到达顺序。

#### Fix Implemented
- `ResolveGeminiCLIProjectID(...)` 改为接收 `GeminiCLIProjectSelection`，显式携带 requested project、response project、tier 和 explicit 标记；只有自动发现、同项目、FREE/LEGACY 或 `gen-lang-client-*` 场景会采用后端项目，付费显式项目保留用户选择。
- CLI 登录与管理端 Gemini OAuth onboarding 调用点统一传入完整项目选择上下文，并在付费显式项目被 Google 返回不同项目时记录保留 requested project 的日志。
- `responsesSSEFramer` 改为按 `response.output_item.done` 到达顺序保存 output item，completed.output 为空时按记录顺序回填，不再按 `output_index` 排序后追加 unindexed item。
- 新增/调整回归测试：
  - `TestResolveGeminiCLIProjectIDPreservesPaidExplicitProject`
  - `TestResolveGeminiCLIProjectIDPrefersBackendProjectForFreeTier`
  - `TestResolveGeminiCLIProjectIDUsesResponseForAutoDiscovery`
  - `TestForwardResponsesStreamRepairsMixedDoneItemsInArrivalOrder`

#### Validation
- `timeout 60s go test ./internal/misc ./sdk/api/handlers/openai -run 'TestResolveGeminiCLIProjectID|TestForwardResponsesStreamRepairs' -count=1`
- `timeout 60s go test ./internal/cmd ./internal/api/handlers/management ./internal/misc ./sdk/api/handlers/openai -count=1`
- `git diff --check`
