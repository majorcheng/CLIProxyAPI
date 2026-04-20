# API Keys review 修复调试记录

## Observations

- `internal/config/sdk_config.go::SDKConfig` 当前把公开字段 `APIKeys` 从 `[]string` 改成了 `[]ClientAPIKey`。
- `sdk/config/config.go` 通过 `type SDKConfig = internalconfig.SDKConfig` 将该结构直接 re-export 给外部 Go 嵌入方，因此旧代码 `sdkconfig.SDKConfig{APIKeys: []string{"k"}}` 会在编译期失败。
- `internal/api/handlers/management/config_lists.go::GetAPIKeys` 当前直接返回 `h.cfg.APIKeys`，即对象数组；而 `internal/tui/client.go::GetAPIKeys` 仍保留“新客户端兼容旧服务端字符串数组”的 fallback，这说明服务端读接口已不再兼容旧客户端。
- `internal/api/handlers/management/config_lists.go::patchClientAPIKeys` 在 index/value 或 old/new 更新后都会调用 `config.NormalizeClientAPIKeys`。
- `internal/config/api_keys.go::NormalizeClientAPIKeys` 当前规则是同名 key 仅保留第一次出现，后续项直接忽略，因此编辑成重名时会产生 200 成功但修改被静默丢弃的问题。
- 当前行为已经覆盖到测试：`internal/api/handlers/management/config_lists_delete_keys_test.go` 假定 GET 返回对象数组；仓内其它测试和初始化已把 `SDKConfig.APIKeys` 改成 `[]sdkconfig.ClientAPIKey`。

## Hypotheses

### H1: 对外公开 SDK 的兼容性回归，根因是直接替换字段类型而非引入兼容桥接（ROOT HYPOTHESIS）
- Supports: `sdk/config/config.go` 直接 re-export `internalconfig.SDKConfig`；字段类型变化会直接暴露给外部调用方。
- Conflicts: 仓内测试都同步改了类型，所以本仓不会编译失败，但外部嵌入方会。
- Test: 恢复 `SDKConfig.APIKeys` 为 `[]string`，新增内部策略字段承载对象配置，再调整内部读取路径，确认仓内编译和测试仍通过。

### H2: management GET 的兼容性回归，根因是读接口硬切输出对象数组，缺少向后兼容序列化
- Supports: `GetAPIKeys` 直接返回对象数组；客户端 fallback 只覆盖“旧服务端 -> 新客户端”，不能覆盖“新服务端 -> 旧客户端”。
- Conflicts: 新 TUI 已经能吃对象数组，但这不能证明旧脚本不受影响。
- Test: 将 `GetAPIKeys` 改为兼容输出纯字符串数组，同时保留 PUT/PATCH 对对象输入的支持；再用新旧解析路径各补一个测试。

### H3: PATCH/新增重名时的静默丢数据，根因是管理接口复用全量 normalize 去重而未显式拦截冲突
- Supports: index/value 更新后会 normalize；normalize 保留首项，后项丢弃，正好导致“编辑为重名后第二项消失”。
- Conflicts: 如果业务明确要求全局去重，可能也可以接受，但当前 200 成功且无提示不成立。
- Test: 在 PATCH/append 前先检查目标 key 是否与其它项冲突；一旦冲突直接返回 409/400，而不是先写入再 normalize。

## Experiments

- 待执行：按 H1/H2/H3 的最小生产修复分别实现，并用定向测试验证回归被消除。

## Root Cause

- 公开 SDK 兼容性回归的根因是：`internal/config/sdk_config.go::SDKConfig.APIKeys` 被直接从 `[]string` 改成 `[]ClientAPIKey`，而 `sdk/config/config.go` 又将该结构直接 re-export，导致外部 Go 嵌入方在编译期立刻失配。
- management GET 回归的根因是：`internal/api/handlers/management/config_lists.go::GetAPIKeys` 直接把内部对象数组暴露成默认输出，没有给旧客户端保留字符串数组读接口。
- PATCH 静默丢数据的根因是：`internal/api/handlers/management/config_lists.go::patchClientAPIKeys` 在写入后直接调用 `internal/config/api_keys.go::NormalizeClientAPIKeys` 去重，而去重策略是“同名只保留首次出现”，导致重名编辑被无提示吞掉。

## Fix

- 恢复公开 `SDKConfig.APIKeys` 的旧形状为 `[]string`，并把对象化策略改存到 `SDKConfig.clientAPIKeyEntries`；通过 `internal/config/api_keys.go::{DecodeClientAPIKeyEntriesFromYAML,ClientAPIKeyEntries,SetClientAPIKeyEntries,FindClientAPIKeyInConfig,ClientAPIKeyValuesFromConfig}` 在加载、运行和路由阶段访问内部策略视图。
- `internal/config/config.go::SaveConfigPreserveComments` 改为走 `internal/config/api_keys.go::MarshalConfigForPersistence`，保证虽然公开字段兼容回退，配置文件仍能按对象语义把 `max-priority` 落盘。
- `internal/api/handlers/management/config_lists.go::GetAPIKeys` 默认恢复旧字符串数组输出，同时新增显式对象输出模式；`putClientAPIKeys/patchClientAPIKeys` 在真正写入前新增重复 key 冲突检查，冲突时返回 `409 api_key_conflict`。
- 新增定向测试验证：公开 SDK 兼容初始化、management 默认/对象输出、重复 key 冲突与 YAML 对象化落盘均已覆盖并通过。

---

# 手动编辑 config.yaml 新增 openai-compatibility provider 未热加载排查记录（2026-04-15）

## Observations

- `/backup/code/CLIProxyAPI/internal/watcher/events.go::start` 当前对配置文件使用 `w.watcher.Add(w.configPath)` 直接监听单个 `config.yaml` 文件，而不是监听其父目录。
- `/backup/code/CLIProxyAPI/internal/watcher/events.go::handleEvent` 仅在 `event.Name == configPath` 且操作为 `Write|Create|Rename` 时才会触发 `scheduleConfigReload()`；`/backup/code/CLIProxyAPI/internal/watcher/config_reload.go::reloadConfigIfChanged` 随后会整文件读取并按 hash 去重，再调用 `reloadConfig()` 完整重载配置。
- `/backup/code/CLIProxyAPI/internal/config/config.go::SanitizeOpenAICompatibility` 会直接丢弃 `base-url` 为空的 openai-compat provider；`ValidateOpenAICompatNames` 会在名称规范化冲突时让整次 `LoadConfig()` 失败。
- `/backup/code/CLIProxyAPI/sdk/cliproxy/service.go::registerModelsForAuth` 对 openai-compat provider 只有在 `compat.Models` 非空时才注册模型；若 provider 存在但 `models` 为空，会清理旧注册而不是暴露一个可用 provider。
- 现场部署文件 `/backup/service/CLIProxyAPI/config.yaml` 当前最近修改时间仍为 `2026-04-14 13:36:45 +0800`；其中 openai-compatibility 片段只有 4 个现存 provider，没有看到“今天手动新增”的条目。
- 现场日志 `/backup/service/CLIProxyAPI/logs/main.log` 最后一批 config watcher 相关日志停在 `2026-04-14 13:36:45`，当时是 management `DELETE /v0/management/openai-compatibility` 触发的 `WRITE -> config successfully reloaded -> full client load complete`，说明“管理端写盘触发 reload”在那次是成立的。
- 当前机器上没有发现正在运行的 CLIProxyAPI 服务实例：`docker ps -a` 中没有 cli-proxy-api 容器，`ss -ltnp` 没有 `:8317` 监听，`ps -ef` 也未见 CLIProxyAPI 进程。

## Hypotheses

### H1: 当前这次“没热加载”首先不是 provider 逻辑问题，而是现场服务实例根本没在运行（ROOT HYPOTHESIS）
- Supports: 当前没有 cli-proxy-api 容器、没有 `:8317` 监听、没有 CLIProxyAPI 进程；现场 `config.yaml` 和 watcher 日志也都停在 2026-04-14。
- Conflicts: 如果用户实际操作的是另一台机器/另一份 config/另一个端口实例，这个观察不能解释那边的现象。
- Test: 先确认用户编辑的就是 `/backup/service/CLIProxyAPI/config.yaml`，并确认对应服务实例当前真实运行位置与端口。

### H2: 即便服务在运行，手动编辑保存方式也可能让 watcher 丢失 config 事件
- Supports: `events.go::start` 当前只 watch 单文件；fsnotify 官方不推荐这样做，编辑器若采用 rename/atomic replace 可能导致 watcher 丢失。
- Conflicts: 现场旧日志里 `WRITE /CLIProxyAPI/config.yaml` 能正常触发 reload，说明“直接写同一文件”路径本身是可工作的。
- Test: 在运行中的实例上对挂载 config 做一次原地写入和一次 rename 覆盖，比较 watcher 日志是否都出现 `config file changed, reloading`。

### H3: reload 可能成功了，但新增 openai-compat provider 在加载阶段被静默丢弃或未注册模型，所以表现为“没生效”
- Supports: `SanitizeOpenAICompatibility` 会丢弃空 `base-url`；`registerModelsForAuth` 在 `models` 为空时不会注册可用模型。
- Conflicts: 这解释的是“未生效”，解释不了“完全没有 watcher 日志”的情况。
- Test: 检查新增 provider 是否至少具备 `name`、非空 `base-url`、非冲突名称，以及非空 `models`。

## Experiments

- 通过 management API 与远端 SSH/容器只读对照，确认 `GET /v0/management/openai-compatibility` 和 `GET /v0/management/config.yaml` 一致返回 6 个 provider，revision 与 `GET /v0/management/config.yaml` 的 sha256 同为 `437ce1a2f37ac70c96356515e557e491bf57a6023c372ce92b0481ffabceac12`。
- 通过 `scp major@172.16.0.1:/data/service/CLIProxyAPI/config.yaml` 与远端 `ssh stat/sha256sum`，确认宿主机 `/data/service/CLIProxyAPI/config.yaml` 只有 4 个 provider，sha256 为 `0b8f555b2b7cf96963342bed87c1e4a6c0d98ad02f392554ea63dfe64436ca9b`，inode 为 `6577228064`。
- 通过 `ssh docker exec cli-proxy-api stat/sha256sum /CLIProxyAPI/config.yaml`，确认容器内配置文件仍是 6 个 provider，sha256 为 `437ce1a2f37ac70c96356515e557e491bf57a6023c372ce92b0481ffabceac12`，inode 为 `6577228062`；结合 `docker inspect cli-proxy-api` 可见当前 compose 把宿主机单个 `config.yaml` bind mount 到容器 `/CLIProxyAPI/config.yaml`。
- 代码实验已落地到当前仓库：将 `internal/watcher/events.go::start` 的配置监听改为监听 `filepath.Dir(configPath)`，并新增真实 `Start()+os.Rename(temp, configPath)` 回归测试，验证编辑器 rename 覆盖保存仍能触发完整热加载。

## Root Cause

- CIC 当前“后端和落盘 `config.yaml` 不同步”的根因是：部署仍使用单文件 bind mount，把宿主机 `/data/service/CLIProxyAPI/config.yaml` 直接挂到容器 `/CLIProxyAPI/config.yaml`；宿主机 SSH/Vim 保存后触发了 inode 替换，导致宿主机路径已经指向新文件，而 Docker 仍把容器内挂载固定在旧 inode 上，所以 management API 与 watcher 继续读取容器内旧配置。
- 仓库层面的次级稳态问题是：`internal/watcher/events.go::start` 过去只监听单个 config 文件；即便容器能看到新的路径内容，编辑器 rename/replace 保存仍可能导致 watcher 后续丢事件。

## Fix

- 部署模板修复：`docker-compose.yml` 改为目录级挂载，把宿主机服务目录挂到容器内独立路径 `/data/service/CLIProxyAPI`，并通过 `command: ["./CLIProxyAPI", "--config", "/data/service/CLIProxyAPI/config.yaml"]` 显式指定配置文件，避免单文件 bind mount 与旧 inode 脱钩。
- watcher 修复：新增 `internal/watcher/watch_targets.go`，由 `addWatchTargets()` 统一在启动时校验配置文件存在、监听配置父目录，并在 `configDir == authDir` 时去重；`internal/watcher/events.go::handleEvent` 保持只认精确 `configPath` 的 `Write|Create|Rename` 事件，`internal/watcher/config_reload.go` 的整文件 hash/full reload 语义保持不变。
- 回归测试：补充 `internal/watcher/watcher_test.go::TestStartAddsConfigDirectoryWatcherAndAuthDirectoryWatcher`、`TestStartDeduplicatesSharedConfigAndAuthDirectoryWatch`、`TestStartWithRenameReplaceConfigTriggersReload`，验证目录监听、共享目录去重以及 rename 覆盖保存热加载。

---

# priority 请求期路由限制排查记录（2026-04-17）

## Observations

- `sdk/api/handlers/handlers.go::applyClientRoutingPolicyMetadata` 会在 `ExecuteWithAuthManager` / `ExecuteCountWithAuthManager` / `ExecuteStreamWithAuthManager` 中把 client key 的 `max-priority` 写入 `max_auth_priority` metadata。
- `sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 当前强依赖 gin context 上的 `accessProvider == config-inline` 和 `apiKey` 字段；只要这两个字段在 live 请求上没有按预期写入，就不会下发 `max_auth_priority`。
- 现场 `/data/service/CLIProxyAPI/config.yaml` 中，`sk-94e0...e8e0` 配置为 `max-priority: 0`，`sk-1f1f...5bc6` 配置为 `max-priority: 100`。
- 现场 `/data/service/CLIProxyAPI/auths/codex-james.one@vmails.net-plus.json` 当前顶层 `priority` 为 `5`；`/data/service/CLIProxyAPI/auths/codex-thomas@vmails.net-plus.json` 当前顶层 `priority` 为 `10`。
- 现场 `/data/service/CLIProxyAPI/logs/main.log` 已确认 `codex-james.one@vmails.net-plus.json` 会被真正选中，但集中出现在 `gpt-5.2`；`codex-thomas@vmails.net-plus.json` 则长期出现在 `gpt-5.4`。

## Hypotheses

### H1: `max-priority` 没成功下发到请求 metadata，根因是 `clientAPIKeyMaxPriority` 过度依赖 gin 上的 `accessProvider/apiKey`（ROOT HYPOTHESIS）
- Supports: 现场 `sk-94e0...e8e0` 已配置 `max-priority: 0`，但请求仍命中 `priority > 0` 的 Codex auth；而 `clientAPIKeyMaxPriority` 当前只有在 gin 中的两个字段同时命中时才会返回上限。
- Conflicts: 还没有在 live 进程里直接打印出“这次请求 gin 没带 accessProvider/apiKey”，因此最后一跳仍是代码推断。
- Test: 将 `clientAPIKeyMaxPriority` 改成优先直接从 HTTP request 提取入站 key，并用 `sdk/config.FindClientAPIKeyInConfig` 对照配置查 `max-priority`；补 Bearer/query key 回归测试。

### H2: `priority=5` 本身没有失效，真正影响选路的是“更高 priority bucket 仍被视为可用”
- Supports: `sdk/cliproxy/auth/selector.go::getAvailableAuths` 与 `sdk/cliproxy/auth/scheduler.go::highestReadyPriorityLocked` 都只会选当前最高 ready priority bucket；只要更高桶仍 ready，5 桶就不会被考虑。
- Conflicts: 这解释的是整体 priority 语义，不能单独解释 `sk-94e0...e8e0` 的 `max-priority: 0` 为什么完全没拦住高桶。
- Test: 继续只读核对更高 priority auth 的模型态/冷却态与实际日志是否一致。

## Root Cause

- 当前已确认的首要根因是：`sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 把 client key 路由限制绑定在 gin 上的 `accessProvider/apiKey` 两个运行时字段上，而不是直接从请求里解析入站 key 再对照配置查策略，导致 live 请求稍有偏差就会完全跳过 `max_auth_priority` 注入。

## Fix

- 将 `sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 改为优先直接从 `ginCtx.Request` 提取候选 client key（`Authorization` / `X-Goog-Api-Key` / `X-Api-Key` / query `key` / query `auth_token`），并用 `sdk/config.FindClientAPIKeyInConfig` 命中配置后返回 `MaxPriority`。
- 保留现有 `applyClientRoutingPolicyMetadata -> max_auth_priority -> shouldSkipAuthByClientPolicy` 下游链路不变，只修正“请求期策略注入”这一跳。
- 增补 `sdk/api/handlers/handlers_request_metadata_test.go` 回归测试，覆盖 Bearer key、query key、未知 key 忽略与 gin fallback。

## 2026-04-17 reviewer 回归复核

### Observations

- `internal/api/server.go::AuthMiddleware` 在鉴权成功时会把真实 principal 写入 gin：`apiKey=result.Principal`、`accessProvider=result.Provider`。
- `internal/access/config_access/provider.go::Authenticate` 会按固定顺序选择“首个命中的 client key”：`Authorization -> X-Goog-Api-Key -> X-Api-Key -> query key -> query auth_token`。
- 上一版修复把 `sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 改成优先扫描整个 request；这会产生两个偏差：
  1. 若 gin 已经记录真实 principal，request 中其它候选 key 仍可能被误拿来绑定 `max_auth_priority`；
  2. 若 `accessProvider != config-inline`，request fast path 仍可能绕过非 inline provider 的忽略语义。

### Root Cause

- 回归根因是：`sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 把“从 request 兜底恢复 principal”放到了“尊重 gin 中真实鉴权结果”之前，导致 request 中额外携带的其它 key 有机会覆盖本次请求真正完成鉴权的 principal。

### Fix

- `sdk/api/handlers/handlers.go::clientAPIKeyMaxPriority` 现已改为：
  1. 先检查 gin 中的 `accessProvider`；若它明确不是 `config-inline`，则直接忽略 `max-priority`；
  2. 若 gin 中已有 `apiKey`，只对这个真实 principal 调用 `lookupClientAPIKeyMaxPriority(...)`；
  3. 只有 gin 尚未提供鉴权结果时，才调用 `clientAPIKeyMaxPriorityFromRequest(...)` 兜底。
- `sdk/api/handlers/handlers.go::clientAPIKeyMaxPriorityFromRequest` 现已改为先通过 `authenticatedClientAPIKeyFromRequest(...)` 按 `config_access/provider.go::Authenticate` 相同顺序恢复“首个命中的 client key”，再查询这个 key 的 `max-priority`；不再扫描到任意带上限的 key 就提前返回。
- `sdk/api/handlers/handlers_request_metadata_test.go` 已新增两条回归测试：
  - `TestApplyClientRoutingPolicyMetadata_UsesAuthenticatedPrincipalFromGin`
  - `TestApplyClientRoutingPolicyMetadata_RequestFallbackKeepsFirstAuthenticatedCandidate`
- `sdk/api/handlers/handlers_request_metadata_test.go::TestApplyClientRoutingPolicyMetadata_IgnoresNonInlineAccessProvider` 也已补成带真实 request 的形态，验证非 inline provider 不会再被 request fallback 绕过。


## 2026-04-17 reviewer 回归修复（空 metadata 跳过 max-priority）

### Observations

- `sdk/api/handlers/handlers.go::requestExecutionMetadata` 现在在请求未携带 `Idempotency-Key`、也没有 execution session / pinned auth 等附加信息时，会返回一个长度为 0 的非 nil metadata map。
- `sdk/api/handlers/handlers.go::applyClientRoutingPolicyMetadata` 仍保留 `if len(meta) == 0 || !ok { return }` 的早退条件，因此这类普通请求即使已经命中受限 client key，也不会写入 `max_auth_priority`。
- `sdk/api/handlers/handlers_request_metadata_test.go` 之前的路由策略测试都手工预填了 `idempotency_key`，没有覆盖“真实请求先走 requestExecutionMetadata，再从空 map 开始写入第一条 metadata”这条路径。

### Hypothesis

#### H1: `applyClientRoutingPolicyMetadata` 错把“空 map”当成“不可写 metadata”，导致 client key 的 `max-priority` 约束在普通请求上被跳过（ROOT HYPOTHESIS）
- Supports: `requestExecutionMetadata` 返回的是可写的空 map，而不是 nil；按当前实现，`max_auth_priority` 本来就可能是该请求写入的第一条 metadata。
- Test: 新增一条回归测试，先构造只带 `Authorization: Bearer key-direct` 的请求，再依次调用 `requestExecutionMetadata` 和 `applyClientRoutingPolicyMetadata`，确认空 map 也能成功写入 `max_auth_priority=5`，同时不伪造 `idempotency_key`。

### Root Cause

- 本次 selective port 把 `requestExecutionMetadata` 收口成“缺少 `Idempotency-Key` 时不再伪造 UUID”，但 `applyClientRoutingPolicyMetadata` 仍沿用旧的 `len(meta) == 0` 早退条件，导致空 metadata map 根本拿不到 `max_auth_priority` 这条首个 metadata，进而让 client key 的 `max-priority` 路由限制在大多数普通请求上失效。

### Fix

- `sdk/api/handlers/handlers.go::applyClientRoutingPolicyMetadata` 已改为仅在 `meta == nil` 或根本未命中 `max-priority` 时才早退；对“长度为 0 的非 nil map”继续写入 `coreexecutor.MaxAuthPriorityMetadataKey`。
- `sdk/api/handlers/handlers_request_metadata_test.go` 已新增 `TestApplyClientRoutingPolicyMetadata_AllowsEmptyMetadataFromRequestExecutionMetadata`，锁定“无 `Idempotency-Key` 的真实请求路径”不再回归。

## 2026-04-17 Qwen 移除 reviewer 回归修复

### Observations

- `internal/registry/models/models.json` 当前仍在 `iflow` 渠道保留 `qwen3-max-preview`，并带有 `thinking.levels`。
- `internal/thinking/provider/iflow/apply.go::isEnableThinkingModel` 在上一版移植里把 `qwen3-max-preview` 从 enable-thinking 白名单删掉了，导致 `internal/thinking/apply.go::ApplyThinking` 接受 suffix / body thinking 配置后，最终不会把它转换成 `chat_template_kwargs.enable_thinking`。
- `sdk/auth/filestore.go::readAuthFile`、`internal/watcher/synthesizer/file.go::synthesizeFileAuths`、`internal/api/handlers/management/auth_files.go::buildAuthFromFileData` 仍会接受 `type: "qwen"` 的旧 auth JSON，因此升级后会把已下线 provider 当成正常凭证继续读入或导入。

### Hypotheses

#### H1: iFlow thinking 回归的根因是把仍属于 `iflow` 目录的 `qwen3-max-preview` 误当成顶层 Qwen provider 一并删掉（ROOT HYPOTHESIS）
- Supports: registry 里该模型仍属于 `iflow`；删除白名单后，thinking 配置会在 `ApplyThinking` 后被静默丢掉。
- Conflicts: 若上游也同步删除了 `iflow` 下这个模型，则白名单删除才合理；但当前分支没有跟进 iFlow 整体移除。
- Test: 恢复 `internal/thinking/provider/iflow/apply.go::isEnableThinkingModel` 对 `qwen3-max-preview` 的支持，并补真实模型 ID 的 thinking 转换测试。

#### H2: 僵尸认证的根因是“读取路径仍接受 qwen，运行路径已拆掉”，导致遗留文件看起来仍是有效 auth（ROOT HYPOTHESIS）
- Supports: reviewer 指到的三处读取/导入链都还接受 `type: "qwen"`；而 executor、refresh lead、model 注册已全部移除。
- Conflicts: 若系统在更上游还有统一 provider 拦截，则这些文件可能只是读入后被再次拒绝；但当前代码里没有这样的统一拒绝。
- Test: 在文件读取、watcher 合成、management 导入三条路径上显式拒绝 `qwen`，并补回归测试，确认不会再把旧文件注册成 auth。

#### H3: 只修 management 导入就足够
- Supports: 用户最容易碰到的是上传旧 auth 文件。
- Conflicts: 旧 `auth-dir` 中历史文件仍会在重启或 watcher 扫描时被读入，不能解决升级场景。
- Test: 否决，不采用。

### Root Cause

- 本次 reviewer 指出的两个问题都成立，而且是两条独立回归：一是 `internal/thinking/provider/iflow/apply.go::isEnableThinkingModel` 误删了仍属于 `iflow` 模型目录的 `qwen3-max-preview`，导致 thinking 配置被静默丢弃；二是旧 `qwen` auth 文件在 `sdk/auth/filestore.go`、`internal/watcher/synthesizer/file.go` 与 `internal/api/handlers/management/auth_files.go` 里仍被接受，形成“读取成功但运行期不可用”的僵尸认证。

### Fix

- 恢复 `internal/thinking/provider/iflow/apply.go::isEnableThinkingModel` 对 `qwen3-max-preview` 的 enable-thinking 适配，并新增 `test/thinking_iflow_qwen3_test.go`，锁定真实模型 ID 的 suffix / body 两条转换路径。
- 新增 `sdk/cliproxy/auth/provider_support.go::ValidatePersistedAuthProvider` 统一拒绝已移除的 `qwen` provider，并接到 `sdk/auth/filestore.go::readAuthFile`、`internal/store/{gitstore,objectstore}.go::readAuthFile`、`internal/watcher/synthesizer/file.go::synthesizeFileAuths`、`internal/api/handlers/management/auth_files.go::{listAuthFilesFromDisk,buildAuthFileEntry,buildAuthFromFileData}`，避免旧文件再被读入、展示或导入成可用 auth。

---

# session sticky 语义排查与移植记录（2026-04-17）

## Observations

- `sdk/cliproxy/auth/conductor.go::executeMixedOnce`、`executeCountMixedOnce`、`executeStreamMixedOnce` 在单次请求内部会把失败 auth 记入 `tried`，并继续挑下一个 auth，因此当前请求内确实可以从 tokenA 切到 tokenB。
- `sdk/cliproxy/auth/conductor.go::publishSelectedAuthMetadata` 只会把本次请求选中的 auth ID 写回 `opts.Metadata[selected_auth_id]`，当前代码里没有任何跨请求 `session -> auth` 持久/运行态绑定逻辑。
- `sdk/cliproxy/auth/selector.go::RoundRobinSelector.Pick` 与 `FillFirstSelector.Pick` 只依据当前 ready 候选集合和本地游标选 auth；`sdk/cliproxy/auth/conductor.go::useSchedulerFastPath` 对内建 selector 会走 scheduler fast path，语义同样是按 ready bucket 重新选，不会记住“上次该 session 成功落在哪个 auth”。
- `sdk/cliproxy/auth/conductor.go::applyAuthFailureState` 会把 408/500/502/503/504 这类暂态故障打成 `Unavailable + NextRetryAfter`；恢复后 auth 会重新回到 ready 集合，所以当前同一 session 后续请求会再次按默认 selector 命中恢复后的 tokenA。
- `sdk/cliproxy/auth/conductor.go::isRequestInvalidError` 会把 caller-side 400/422 识别为 request-invalid 并阻断继续重试；模型支持类 400 保持可继续切 auth。用户提到的“400 出错后切到 B”只在这类可切换 400 上成立。
- 上游 `upstream/main` 仍保留完整 session-affinity 能力：`internal/config/config.go::RoutingConfig` 暴露 `session-affinity` / `session-affinity-ttl` / `claude-code-session-affinity`，`sdk/cliproxy/auth/selector.go::SessionAffinitySelector.Pick` 会在绑定 auth 不可用时 fallback 选新 auth 并把 cache 立刻重写到新 auth。
- 当前分支已经有两个相关前提：`sdk/api/handlers/handlers.go::requestExecutionMetadata` 已不再伪造 `Idempotency-Key`，且 `sdk/api/handlers/handlers.go::WithExecutionSessionID` / `internal/runtime/executor/codex_request_plan.go::codexPromptCacheID` 已分别提供 execution session 与 prompt cache continuity 的局部连续性支持。

## Hypotheses

### H1: 当前 sticky 缺口的根因是“缺少跨请求 session->auth 绑定”，导致 A 故障期间切到 B 只在当前请求生效，下一请求又会按默认 selector 从恢复后的 A 重新开始（ROOT HYPOTHESIS）
- Supports: 当前分支没有 `SessionAffinitySelector`、`SessionCache`、`session-affinity` 配置项，也没有在请求成功后把 session 绑定改写到成功 auth 的代码路径。
- Conflicts: 对显式 `PinnedAuthMetadataKey` 或 websocket execution session 这类调用方自带 pin 的请求，当前已经可以保持同一 auth；但这只覆盖局部调用路径。
- Test: 移植 session-affinity 后补一条回归：同一 session 第一次请求 A 失败、B 成功，第二次请求在 A 恢复后仍继续命中 B。

### H2: 直接把上游 session-affinity selector 原样搬进当前分支会打断 success-rate / simhash / priority-zero 等现有扩展选路语义
- Supports: 当前分支比上游新增了 `success-rate`、`simhash`、`priority-zero-strategy`，而 `ensureRequestSimHashMetadata` 目前只识别裸 `*SimHashSelector`；`useSchedulerFastPath` 也只对裸 `RoundRobin/FillFirst` 开 fast path。
- Conflicts: 若我们给包装器补 `UnwrapSelector` / metadata 透传 / legacy 局部覆盖，能够在最小改动下保留这些语义。
- Test: 包装 simhash 时继续写入 `request_simhash`；round-robin/fill-first + session-affinity 时继续保住 priority-zero 覆盖；success-rate 包装后继续透传 `ObserveResult`。

### H3: 只做 Codex continuity key 继续不改 sticky routing，也能满足这次目标
- Supports: `internal/runtime/executor/codex_request_plan.go::codexPromptCacheID` 已提供 prompt cache continuity。
- Conflicts: continuity key 只保证同一 auth 上游 cache identity 稳定，不能决定“后续请求继续落到 B”；用户目标是 auth 绑定迁移后保持新 auth。
- Test: 否决，不采用。

## Root Cause

- 当前 sticky 缺口的根因是：本仓运行态只有“单次请求内失败后继续尝试其它 auth”的能力，没有“同一 session 成功切到新 auth 后，把后续请求继续绑定到这个成功 auth”的跨请求绑定层，因此 A 短暂故障恢复后，同一 session 会重新回到 A，直接损失 prompt cache / 上下文连续性的收益。

## Fix

- 移植上游 `SessionAffinitySelector`、`SessionCache`、多格式 session ID 提取、`session-affinity` / `session-affinity-ttl` / `claude-code-session-affinity` 配置入口与热重载支持。
- 在当前分支额外补一层“成功后立即重绑”语义：请求最终成功落到某个 auth 时，根据已提取的 session ID 立刻刷新 `session -> auth` 映射，确保 A 失败切到 B 后，后续继续稳定落到 B。
- 为保持当前分支扩展语义，额外补三个适配点：
  1. `simhash` 包装时继续识别并注入 `request_simhash`；
  2. `success-rate` 包装时继续透传 `ObserveResult`；
  3. `round-robin/fill-first + priority-zero-strategy` 与 session-affinity 组合时，在 legacy 路径补一个局部 priority-zero 覆盖包装器。

---

# 2026-04-17 session affinity reviewer 回归修复

## Observations

- `sdk/cliproxy/auth/session_affinity.go::pickAndBind` 与 `pickFromFallbackCache` 会在 selector 选出 auth 后立刻写 `SessionCache`，请求执行结果尚未产生。
- `sdk/cliproxy/auth/conductor.go::{executeMixedOnce,executeCountMixedOnce,executeStreamMixedOnce}` 已经在成功返回路径调用 `bindSessionAffinityFromMetadata(...)`，成功态重绑能力本来就存在。
- `sdk/cliproxy/auth/conductor.go::isBuiltInSelector` 与 `sdk/cliproxy/auth/scheduler.go::selectorStrategy` 只识别裸 `*RoundRobinSelector` / `*FillFirstSelector`，开启 sticky 包装后 mixed 请求会脱离 scheduler fast path。
- `sdk/cliproxy/auth/scheduler.go::pickMixed` 保留 mixed fill-first 的 provider 顺序和 Codex websocket 优先语义；`sdk/cliproxy/auth/conductor.go::pickNextMixedLegacy` 使用全局扁平候选集合。

## Hypotheses

### H1: 失败请求污染 session cache，根因是 selector 选中时机与成功落地时机混在一起（ROOT HYPOTHESIS）
- Supports: `pickAndBind`/`pickFromFallbackCache` 先写 cache，`executeMixedOnce` 成功路径再次写 cache。
- Conflicts: 缓存命中需要继续刷新 TTL，这部分能力仍要保留。
- Test: 去掉 Pick 阶段写缓存，只保留缓存命中时的 TTL 刷新与成功路径 `BindSelectedAuth` 写入；新增“失败请求不建绑定”测试。

### H2: mixed fill-first 语义漂移，根因是 sticky 包装让 built-in selector 失去 scheduler fast path 身份
- Supports: `isBuiltInSelector` 与 `selectorStrategy` 直接看具体类型，包装后走 legacy mixed path。
- Conflicts: 单 provider 场景仍会保持正确，因为 scheduler/legacy 在单 provider 上差异较小。
- Test: 让 built-in 检测支持 unwrap 包装层，并新增 mixed fill-first provider 顺序回归测试。

### H3: mixed websocket 优先语义漂移，根因与 H2 相同，codex websocket 偏好只在 scheduler mixed path 生效
- Supports: `pickMixed` 里有 `preferWebsocketForProvider(providerKey)`；legacy mixed path 传入 provider=`mixed`。
- Conflicts: 单 provider 的直接 selector 仍会保留 `preferCodexWebsocketAuths`。
- Test: 在 sticky+fill-first 下新增 mixed codex websocket 回归测试。

## Experiments

- 按 H1 修改 `sdk/cliproxy/auth/session_affinity.go`：`Pick` 只读取绑定并刷新命中 TTL，新的绑定统一通过成功路径 `BindSelectedAuth` 落缓存；新增 `TestSessionAffinitySelector_FailedPickDoesNotCreateBinding` 与 TTL 刷新测试验证行为。
- 按 H2/H3 修改 `sdk/cliproxy/auth/selector_wrappers.go`、`sdk/cliproxy/auth/scheduler.go`、`sdk/cliproxy/auth/conductor.go`：built-in 判定和 scheduler strategy 统一支持 unwrap 包装层；新增 wrapped fill-first mixed provider 顺序与 codex websocket 回归测试。

## Root Cause

- reviewer 指出的三条回归共用两处根因：一是 `SessionAffinitySelector` 在请求成功前提前写入 cache，二是 built-in selector 识别没有穿透包装层，导致 mixed 请求离开 scheduler fast path。

## Fix

- `sdk/cliproxy/auth/session_affinity.go::Pick` 现在只消费已存在的成功绑定；primary/fallback cache 命中继续刷新 TTL，新绑定统一由 `BindSelectedAuth` 在成功路径写入，失败请求不会再污染 sticky 关系。
- `sdk/cliproxy/auth/selector_wrappers.go::builtInSelectorStrategy` 提供统一 unwrap 能力，`sdk/cliproxy/auth/scheduler.go::{newAuthScheduler,selectorStrategy}` 与 `sdk/cliproxy/auth/conductor.go::isBuiltInSelector` 改为复用该能力，因此 `SessionAffinitySelector`、`PriorityZeroOverrideSelector` 等包装后的 RR/FF 继续保留 scheduler fast path 语义。
- 新增回归测试覆盖：失败请求不建绑定、成功绑定后的 TTL 刷新、wrapped fill-first mixed provider 顺序、wrapped fill-first mixed codex websocket 偏好。


# 2026-04-18 v6.9.29 selective port 测试失败排查

## Observations

- 新增定向测试 `internal/api/handlers/management/config_auth_index_test.go::TestGetOpenAICompatIncludesEntryAndFallbackAuthIndex` 首次运行直接 panic，报错为 `testing: test using t.Setenv ... can not use t.Parallel`。
- 触发点在 `internal/api/handlers/management/openai_compat_management_test.go::newOpenAICompatTestHandler`；该 helper 内部调用了 `t.Setenv("MANAGEMENT_PASSWORD", "")`。
- 失败测试当前声明了 `t.Parallel()`，因此在测试框架层就被拒绝，和业务实现本身无关。

## Hypotheses

### H1: 测试失败的根因是并行测试与 `t.Setenv` 组合冲突（ROOT HYPOTHESIS）
- Supports: panic 栈直接落在 `testing.(*T).Setenv`，并明确指出与 `t.Parallel` 组合非法。
- Conflicts: 暂无。
- Test: 去掉该测试的 `t.Parallel()`，重新跑同一组 management 定向测试。

### H2: helper 需要重写成不使用 `t.Setenv`
- Supports: 这样可以继续保留并行。
- Conflicts: 现有仓库已有多处直接复用该 helper，重写范围更大，超出本次最小修补边界。
- Test: 当前先不采用，保留为备选。

### H3: panic 只是第一层，后面还会有业务断言失败
- Supports: 当前只看到框架级 panic，还没真正执行到业务断言。
- Conflicts: 需要重新跑测试才能确认。
- Test: 去掉 `t.Parallel()` 后继续跑原命令。

## Experiments

- 实验 1：按 H1 去掉 `TestGetOpenAICompatIncludesEntryAndFallbackAuthIndex` 的 `t.Parallel()`，再重跑 management 定向测试；结果：panic 消失，原命令通过，说明这是纯测试框架约束问题。

## Root Cause

- 本轮测试失败的根因是：`TestGetOpenAICompatIncludesEntryAndFallbackAuthIndex` 复用了 `newOpenAICompatTestHandler`，而该 helper 内部调用 `t.Setenv`；Go 1.26 测试框架明确禁止 `t.Setenv` 与 `t.Parallel` 组合，因此测试在进入业务断言前就被框架直接 panic。

## Fix

- 去掉 `internal/api/handlers/management/config_auth_index_test.go::TestGetOpenAICompatIncludesEntryAndFallbackAuthIndex` 的 `t.Parallel()`，继续复用现有 helper，保持本轮修补范围只落在 v6.9.29 selective port 主线与最小测试修正。


# 2026-04-18 reviewer 修复阶段测试编译失败排查

## Observations

- reviewer 修复后首次运行 management 定向测试时，编译器报错 `internal/api/handlers/management/config_auth_index_test.go:240:86: undefined: bytes`。
- 新增失败点位于 `TestPatchOpenAICompatResponseIncludesAuthIndex`，该测试使用了 `bytes.NewBufferString(...)` 构造 PATCH 请求体。
- 当前 `internal/api/handlers/management/config_auth_index_test.go` 的 import 列表里缺少 `bytes`。

## Hypotheses

### H1: 根因是测试新增了 `bytes.NewBufferString`，但忘了补 `bytes` import（ROOT HYPOTHESIS）
- Supports: 编译报错直接指向 `undefined: bytes`，且测试正文确实新用了 `bytes.NewBufferString`。
- Conflicts: 暂无。
- Test: 给 `config_auth_index_test.go` 补 `bytes` import，重跑同一条 management 定向测试命令。

### H2: 其它测试文件也有隐藏的 import/命名冲突
- Supports: 当前只跑到第一处编译错误，后续问题还没暴露。
- Conflicts: 需要先修复 H1 后再验证。
- Test: 修完 H1 后继续重跑原命令。

## Experiments

- 实验 1：按 H1 给 `config_auth_index_test.go` 补 `bytes` import，再重跑 management 定向测试，观察是否进入业务断言。


# 2026-04-18 reviewer fix 二次排查

## Observations

- management 定向测试 `TestAPIKeysManagement_DefaultsToLegacyStringsButSupportsObjectFormatAndPatchDelete` 当前失败，GET `?format=object` 返回 `{"api-keys":[{"key":"alpha"},{"key":"beta"}]}`，缺少 `beta.max-priority=0`。
- 读取链已从 `internal/api/handlers/management/config_lists.go::GetAPIKeys` 切到 `internal/api/handlers/management/handler.go::currentConfigSnapshot`。
- `currentConfigSnapshot` 当前用 `yaml.Marshal` / `yaml.Unmarshal` 克隆 `h.cfg`；而 `internal/config/sdk_config.go::SDKConfig.clientAPIKeyEntries` 带 `yaml:"-"`，对象化 `api-keys` 视图在快照里会丢失。
- management 包内仍有多条直接读取 `h.cfg` / `h.authManager` 的路径，reviewer 点名的热更新竞态风险在 `config_basic.go`、`logs.go`、`quota.go`、`usage.go`、`oauth_callback.go`、`vertex_import.go`、`api_tools.go`、`auth_files.go` 等文件里仍可见。

## Hypotheses

### H1: `currentConfigSnapshot` 丢掉 `clientAPIKeyEntries`，导致对象化 GET 响应退化成纯字符串视图（ROOT HYPOTHESIS）
- Supports: 失败只出现在 `?format=object`，缺失字段正好来自 `clientAPIKeyEntries.MaxPriority`。
- Conflicts: 纯字符串 GET 仍然正确，说明公开 `APIKeys` 视图没有损坏。
- Test: 在快照阶段显式恢复 `SetClientAPIKeyEntries(h.cfg.ClientAPIKeyEntries())`，重跑同一条 management 定向测试。

### H2: reviewer 高优先级发现继续成立，根因是 management 读路径仍大量绕过 `currentConfigSnapshot/currentAuthManager`
- Supports: grep 结果仍能看到多个 handler/helper 直接访问 `h.cfg` / `h.authManager`。
- Conflicts: reviewer 明确点名的 `Middleware`、`managementCallbackURL`、`ListAuthFiles`、`authByIndex` 已修。
- Test: 把剩余公开 handler 与核心 helper 收口到快照/manager accessor，随后跑 management 定向测试与全仓编译验证。

## Experiments

- 实验 1：先修 `currentConfigSnapshot` 的 `api-keys` 对象视图恢复，再继续收口剩余 management 读路径，之后重跑 reviewer 相关 management 定向测试。


## 2026-04-19 v6.9.30 Codex 图片生成 translator 调试记录

### Observations
- 定向测试 `timeout 60s go test ./internal/translator/codex/gemini ./internal/translator/codex/openai/chat-completions -run 'Image|EmptyOutput|ToolCall|FirstChunk|SetsToolCallsFinishReason' -count=1` 首轮失败点固定在 `internal/translator/codex/gemini/codex_gemini_response_test.go::TestConvertCodexResponseToGemini_NonStreamImageGenerationCallAddsInlineDataPart`。
- 失败现象是 `inlineData.data = ""`，说明流式路径已写入图片逻辑，Gemini non-stream 路径仍未把 `image_generation_call` 落到 `candidates.0.content.parts`。
- `internal/translator/codex/gemini/codex_gemini_response.go::ConvertCodexResponseToGeminiNonStream` 的 `switch itemType` 现场只有 `reasoning` / `message` / `function_call` 三支，缺少 `image_generation_call`。

### Hypotheses
- H1: Gemini non-stream 漏掉 `case "image_generation_call"`，因此最终响应不会写入 `inlineData`。
  - Supports: 失败只出现在 Gemini non-stream；源码 switch 缺失该分支。
  - Conflicts: 无。
  - Test: 只补一个 `case "image_generation_call"`，再跑同一组定向测试。
- H2: 测试断言路径写错，真实图片被写到了其它字段。
  - Supports: 无。
  - Conflicts: `sed` 现场能看到 non-stream 根本没有图片分支。
  - Test: 打印完整输出 JSON 并查找 `inlineData`。
- H3: `mimeTypeFromCodexOutputFormat` 返回空导致断言链路中断。
  - Supports: 失败值是空字符串。
  - Conflicts: `inlineData.data` 为空早于 mimeType 参与。
  - Test: 单独断言 data 字段是否进入模板。

### Experiments
- 实验 E1：只读检查 `ConvertCodexResponseToGeminiNonStream` 的 `switch itemType`。结果：确认缺少 `image_generation_call` 分支，支持 H1。
- 实验 E2：最小补丁加入 `case "image_generation_call"`，把 `result` 转成 `inlineData` part。结果：待重新跑原失败测试验证。

### Root Cause
- Gemini non-stream translator 漏掉了 `image_generation_call` 分支，导致最终图片响应没有写入 `inlineData` part。

### Fix
- 在 `internal/translator/codex/gemini/codex_gemini_response.go::ConvertCodexResponseToGeminiNonStream` 增加 `image_generation_call` 分支，并复用同一 `mimeTypeFromCodexOutputFormat` 助手生成 `inlineData`。


## 2026-04-20 reviewer fix：v6.9.30 图片 translator 二次调试

### Observations
- reviewer 已确认两条可复现缺陷：
  1. `internal/translator/codex/openai/chat-completions/codex_openai_response.go::ConvertCodexResponseToOpenAI` 把 `imageURL` 直接拼进 `sjson.SetRaw(...)` 的 JSON 字符串，`output_format` 若含引号或反斜杠，会生成非法 JSON。
  2. `internal/translator/codex/gemini/codex_gemini_response.go::ConvertCodexResponseToGemini` 在 `partial_image` / `image_generation_call` 分支直接早返回，绕过了函数尾部 `LastStorageOutput` flush，tool call 与图片事件顺序会漂移。
- 当前工作树里 5 个变更文件都与上一轮 `v6.9.30` selective port 相关，本轮边界继续收口在这 4 个 translator 源码/测试文件与 `DEBUG.md`。

### Hypotheses
- H1: OpenAI 流式图片分支的根因是使用字符串拼接构造 `choices.0.delta.images`，因此任何未经 JSON 转义的 `imageURL` 都可能破坏 chunk 结构。
  - Supports: reviewer 已给出带引号的 `output_format` 最小复现。
  - Conflicts: 无。
  - Test: 改成先构造 `imagePayload` 再用 `sjson.Set` / `SetRaw` 注入，新增带引号 `output_format` 的测试。
- H2: Gemini 顺序问题的根因是图片分支直接 `return`，没有走尾部 `LastStorageOutput` flush。
  - Supports: 现有函数只在统一尾部 flush；图片分支都提前返回。
  - Conflicts: 无。
  - Test: 增加 `function_call done -> partial_image -> completed` 顺序测试，修复后应先吐 tool call 再吐图片。
- H3: 只在图片分支前局部 flush 就足够，无需重构整个返回协议。
  - Supports: 当前功能缺口集中在两条早返回路径。
  - Conflicts: 若后续再新增类似早返回事件，还会重复踩坑。
  - Test: 提炼一个小 helper 统一“带 flush 返回”的逻辑，复用到两条图片分支。

### Experiments
- E1: 先做只读核对，确认 OpenAI 流式图片路径当前仍用 `template + imageURL + SetRaw(...)` 形式构造 `delta.images`。
- E2: 只读核对 Gemini 流式函数，确认 `LastStorageOutput` 只在函数尾部 flush，图片分支当前直接返回。

### Root Cause
- OpenAI 流式图片 chunk 使用字符串拼接构造 JSON，且 Gemini 图片分支在早返回时绕过了 `LastStorageOutput` flush。

### Fix
- OpenAI 流式图片改为 `buildImageDeltaChunk(...)` 逐字段写入；Gemini 图片返回改为统一走 `withPendingStorageOutput(...)`，先输出缓存 tool call 再输出图片 chunk。


## 2026-04-20 PR2266 websocket compact replay 400 排查

### Observations
- 参考 PR `router-for-me/CLIProxyAPI#2266` 的说明，本次 400 `No tool output found for function call ...` 对应的是 websocket compact 之后客户端回放整段 transcript 时，旧的 `lastRequest` / `lastResponseOutput` 与新 transcript 继续合并，导致 `function_call` / `function_call_output` 配对被打乱。
- 当前本地实现位于 `sdk/api/handlers/openai/openai_responses_websocket.go::normalizeResponseSubsequentRequest`，它会先调用 `shouldReplaceWebsocketTranscript(rawJSON, nextInput)`；只有命中“input 中含 `function_call` 或 `message(role=assistant)`”才走 transcript replacement，其余情况继续走 merge。
- 当前本地 `sdk/api/handlers/openai/openai_responses_websocket.go::shouldReplaceWebsocketTranscript` 没有识别 `compaction` / `compaction_summary`，也没有识别 `custom_tool_call`，因此“compact 后只回放 `message + compaction`”会继续落入 merge 分支。
- 现场实验：临时新增定向测试后调用 `normalizeResponsesWebsocketRequest(...)`，输入为 `lastRequest + lastResponseOutput + nextInput=[message,user,msg-2 ; compaction]`，当前结果仍返回 6 个 merged items，说明 stale merge 仍在发生；临时测试已删除。
- `internal/translator/claude/openai/responses/claude_openai-responses_request.go` 与 `internal/translator/gemini/openai/responses/gemini_openai-responses_request.go` 的 request translator 只消费 `message` / `function_call` / `function_call_output` 主线，`compaction` / `compaction_summary` 不会直接下沉，因此 compact replay bypass 需要按 downstream 能力门控。
- 当前仓库已经包含更早一批相关修复：`sdk/api/handlers/openai/openai_responses_websocket.go::shouldReplaceWebsocketTranscript`、`normalizeResponseTranscriptReplacement`、`repairResponsesWebsocketToolCalls` 以及 `sdk/api/handlers/openai/openai_responses_websocket_toolcall_repair.go`，说明本轮缺口集中在 compact replay 检测与 downstream 门控，而不是整套 websocket tool repair 缺失。

### Hypotheses
- H1 ROOT: `sdk/api/handlers/openai/openai_responses_websocket.go::shouldReplaceWebsocketTranscript` 缺少对 `compaction` / `compaction_summary` 的识别，导致 compact replay 继续 stale merge，最终打乱 tool call/output 配对。
  - Supports: PR2266 描述与当前源码差异直接对上；本地实验已复现 merge 仍发生。
  - Conflicts: 当前已有 assistant/function_call replacement 逻辑，部分 compact 形态已经被覆盖。
  - Test: 为 `normalizeResponsesWebsocketRequestWithMode` 增加“compaction replay + codex 支持 bypass”与“unsupported downstream fallback merge”两组测试。
- H2: compact replay bypass 需要限定在支持该语义的 downstream；若对 Claude/Gemini 一律 bypass，translator 会丢弃 compaction items，导致上下文丢失。
  - Supports: 两个 translator 的 switch 未处理 `compaction` / `compaction_summary`。
  - Conflicts: Codex 路径本身已支持 compact 语义，不能因为其他 downstream 而关闭整个 bypass。
  - Test: 增加 `websocketUpstreamSupportsCompactionReplayForModel` 与 fallback strip-compaction 测试。
- H3: 当前仓库对 `custom_tool_call` transcript replacement 覆盖不足，可能与同类 400 共因。
  - Supports: upstream/dev 的 `shouldReplaceWebsocketTranscript` 已把 `custom_tool_call` 纳入 replacement 信号；当前本地只识别 `function_call`。
  - Conflicts: 本次用户给的错误文案是 `No tool output found for function call ...`，主链更贴近标准 function_call compact replay。
  - Test: 核对现有 `openai_responses_websocket_test.go` 是否已有 custom tool transcript reset 覆盖，必要时一并补齐。
