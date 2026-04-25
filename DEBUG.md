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

# RT 交换失败后 priority 未收口到 5 排查记录（2026-04-20）

## Observations

- `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 当前在 `exec.Refresh(...)` 返回错误后，只更新 `LastError`、`NextRefreshAfter`、`UpdatedAt` 与 initial refresh pending 标记。
- `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 当前错误分支里没有任何 `priority` 写回逻辑。
- `sdk/cliproxy/auth/selector.go::authPriority` 与 `sdk/cliproxy/auth/scheduler.go::highestReadyPriorityLocked` 都读取 `Auth.Attributes["priority"]` 作为运行期调度优先级。
- `sdk/auth/filestore.go::Save` 通过 `sdk/cliproxy/auth/runtime_state_persistence.go::MetadataWithPersistedRuntimeState` 把 `Auth.Metadata` 落盘，因此要让 auth JSON 顶层出现 `"priority": 5`，需要同步写入 `Auth.Metadata["priority"]`。
- `internal/api/handlers/management/auth_files.go::PatchAuthFileFields` 已经给出 priority 双写语义：同时更新 `Auth.Attributes["priority"]` 与 `Auth.Metadata["priority"]`。

## Hypotheses

### H1: live 未生效的根因是 `executeRefreshAuth` 错误分支压根没有写回 priority（ROOT HYPOTHESIS）
- Supports: 当前实现只有失败日志、退避与错误态更新；代码里搜不到 RT 失败后的 priority 写入。
- Conflicts: 暂无。
- Test: 在错误分支里给带 `refresh_token` 的 auth 同时写 `Attributes["priority"]="5"` 与 `Metadata["priority"]=5`，再补定向测试验证内存态与异步落盘都变成 5。

### H2: priority 已写到运行态，只有落盘路径丢了
- Supports: 调度读 `Attributes`，落盘读 `Metadata`，两套路径分离。
- Conflicts: 当前错误分支连运行态 `Attributes["priority"]` 都没改。
- Test: 回归测试同时断言运行态与持久化快照。

### H3: management 展示链忽略了 priority
- Supports: 用户看到的是文件/管理面结果。
- Conflicts: `internal/api/handlers/management/auth_files.go::buildAuthFileEntry` 已优先读 `Attributes["priority"]`，其次读 `Metadata["priority"]`，展示链本身具备读取能力。
- Test: 优先修正写路径，再用现有测试验证双写结果。

## Experiments

- 只读核对 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth`，确认 RT 失败后当前实现缺少 priority 写回。
- 只读核对 `sdk/cliproxy/auth/selector.go::authPriority`、`sdk/cliproxy/auth/scheduler.go::highestReadyPriorityLocked`、`internal/api/handlers/management/auth_files.go::PatchAuthFileFields` 与 `sdk/auth/filestore.go::Save`，确认本次修复应采用 `Attributes + Metadata` 双写。
- 生产修复：在 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的失败分支新增 `setPreferredPriorityAfterRTExchangeFailure(...)` 收口逻辑，把 RT 失败后的 auth 统一写成 `priority=5`，并通过异步持久化把快照落盘。
- 回归测试：扩展 `sdk/cliproxy/auth/conductor_refresh_failure_test.go`，覆盖 terminal 401、transient 429 与 `RefreshAuthNow(...)` 三条路径，确认 RT 失败后运行态与持久化快照都变成 5。

## Root Cause

- 根因是 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的 refresh 失败分支缺少 priority 写回逻辑，导致 RT 交换失败后只更新错误态与退避时间，既没有把运行态调度 priority 收口到 5，也没有把 `"priority": 5` 持久化回 auth 文件。

## Fix

- 新增 `sdk/cliproxy/auth/conductor.go::{setPreferredPriorityAfterRTExchangeFailure,authMetadataPriorityEquals}`，把 RT 失败后的优先级收口统一封装成双写 helper。
- 在 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的错误分支中，只要当前 auth 携带 `refresh_token`，就把 `Auth.Attributes["priority"]` 写成 `"5"`、把 `Auth.Metadata["priority"]` 写成 `5`，并复用既有异步持久化队列落盘。
- 扩展 `sdk/cliproxy/auth/conductor_refresh_failure_test.go::{TestManagerRefreshAuth_PersistsTerminalRefresh401ForMaintenance,TestManagerRefreshAuth_DoesNotMarkTransientRefreshStatusForMaintenance,TestManagerRefreshAuthNow_LogsRTExchangeFailure}`，锁定 401/429/即时刷新三条失败路径上的 priority 收口语义。

## Reviewer Follow-up

- reviewer 指出 `sdk/cliproxy/auth/types.go::Auth` 已把 `Attributes` 定义成 immutable configuration，`internal/api/handlers/management/auth_files.go::PatchAuthFileFields` 也把 priority 双写限定在显式管理入口。
- `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的职责边界适合维护 `LastError`、`NextRefreshAfter`、pending 标记等运行态信息；把一次 refresh 失败升级成 priority 持久化改写，会放大成 operator 路由配置变更。
- 本仓 `tasks/lessons.md` 已同步沉淀：refresh/RT 交换失败场景保持运行态语义，priority 继续由显式管理路径负责。

## Corrective Fix

- 回退 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 中 RT 失败后的 priority 双写与持久化改写逻辑，恢复到只更新 `LastError`、`NextRefreshAfter`、`UpdatedAt` 与 initial refresh pending 标记。
- 调整 `sdk/cliproxy/auth/conductor_refresh_failure_test.go::{TestManagerRefreshAuth_PersistsTerminalRefresh401ForMaintenance,TestManagerRefreshAuth_DoesNotMarkTransientRefreshStatusForMaintenance,TestManagerRefreshAuthNow_LogsRTExchangeFailure}`，改为锁定“refresh 失败后 operator 预设的 priority 保持原值”。

## User Decision Follow-up

- 用户随后把行为边界重新明确为：只有“RT 交换失败的 401”需要把 priority 改成 5。
- 这次约束比“所有 refresh 失败都改 priority”更窄，因此修复收口为 `rtExchangeLogged && terminalStatus == 401`。
- `429` 和普通 refresh 错误继续保持原优先级，减少额外影响面。

## Final Fix

- 在 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 中恢复 priority 双写逻辑，但触发条件只保留 `RT 交换失败 + HTTP 401`。
- `sdk/cliproxy/auth/conductor_refresh_failure_test.go` 现已形成三层语义：
  - `TestManagerRefreshAuth_PersistsTerminalRefresh401ForMaintenance`：401 时 priority 收口到 5；
  - `TestManagerRefreshAuth_DoesNotMarkTransientRefreshStatusForMaintenance`：429 时 priority 保持原值；
  - `TestManagerRefreshAuthNow_LogsRTExchangeFailure` 与 `TestManagerRefreshAuthNow_PromotesPriorityOnRTUnauthorized401`：分别锁定非 401 与 401 的 `RefreshAuthNow(...)` 行为边界。

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

## 2026-04-20 RT 交换失败后 priority=5 收口

### Observations
- `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 当前在 `err != nil` 分支只会写 `LastError`、`NextRefreshAfter` 与初始 refresh pending 清理，未改动 `Auth.Attributes["priority"]` 或 `Auth.Metadata["priority"]`。
- `sdk/cliproxy/auth/selector.go::authPriority` 与 `sdk/cliproxy/auth/scheduler.go::highestReadyPriorityLocked` 都按更高 priority bucket 优先选路，因此运行期把凭证写到 `priority=5` 会直接影响“优先使用”顺序。
- `internal/api/handlers/management/auth_files.go::PatchAuthFileFields` 当前更新优先级时会同时写 `Metadata["priority"]` 与 `Attributes["priority"]`，这给了本次运行期写回的最小对齐语义。

### Hypotheses
- H1: RT 交换失败后没有把 auth 收口到 `priority=5`，因此后续调度仍沿用旧优先级，缺少“优先使用”效果。
  - Supports: `executeRefreshAuth` 失败分支当前没有任何 priority 写回逻辑。
  - Conflicts: 无。
  - Test: 在 `executeRefreshAuth` 失败分支最小加入 priority 写回，补测试验证内存态与持久化快照都变成 5。
- H2: 只写 `Metadata["priority"]` 就足够，因为 management 展示层已有 metadata fallback。
  - Supports: `buildAuthFileEntry` 对 `priority` 已支持 metadata fallback。
  - Conflicts: 运行期调度读取的是 `Attributes["priority"]`。
  - Test: 同时检查调度读路径与 management 写路径，确认需要双写。
- H3: 只写 `Attributes["priority"]` 就足够，因为调度只认 Attributes。
  - Supports: `authPriority` 当前只看 `Attributes`。
  - Conflicts: 持久化 token 文件走 metadata 合并，单写 Attributes 会丢失重启后磁盘语义。
  - Test: 参考 management 既有写法，双写 Attributes 与 Metadata。

### Experiments
- E1: 只读检查 `executeRefreshAuth` 失败分支。结果：确认当前只写错误状态与退避时间，支持 H1。
- E2: 只读检查 management 的 `PatchAuthFileFields`。结果：确认 priority 更新现有约定是同时写 `Metadata` 与 `Attributes`，支持 H2/H3 的双写方案。

### Root Cause
- RT 交换失败分支缺少 priority 收口逻辑，导致 auth 无法在失败后立刻进入 `priority=5` 的优先使用桶。

### Fix
- 在 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的失败分支加入 `priority=5` 双写收口，并补 `sdk/cliproxy/auth/conductor_refresh_failure_test.go` 回归测试，锁定内存态与持久化快照都会写入该优先级。


## 2026-04-20 Chat Completions 伪装 tool result 400 排查

### Observations
- `/tmp/error-v1-chat-completions-2026-04-20T120904-c6f60ac3.log` 的 `=== REQUEST BODY ===` 里，`messages[29]` 是 assistant tool call，包含 `tool_calls[0].id = call5u6VTmiKVl09FPPXrpMKxV8T`。
- 同一请求里的 `messages[30]` 已经带了这次调用的结果内容，但消息形态是 `role="user"`，`content` 为字符串前缀 `[Tool result for call5u6VTmiKVl09FPPXrpMKxV8T]: ...`。
- `sdk/api/handlers/openai/openai_handlers.go::ChatCompletions` 当前会在 `shouldTreatAsResponsesFormat(rawJSON)` 为真时才切到 Responses-format 处理；这次请求携带的是 `messages`，所以仍走 Chat Completions translator 链。
- `internal/translator/codex/openai/chat-completions/codex_openai_request.go::ConvertOpenAIRequestToCodex` 当前只会把 `role="tool"` 且带 `tool_call_id` 的消息翻译成 `function_call_output`。上述 `messages[30]` 会继续作为普通 user message 写入 `input`。
- 现有标准路径已经有回归覆盖：`internal/translator/codex/openai/chat-completions/codex_openai_request_test.go::TestToolCallSimple` 会验证 `role="tool" + tool_call_id` 能翻译成 `function_call_output`。

### Hypotheses
- H1 ROOT: Chat Completions translator 缺少对 `[Tool result for <call_id>]: ...` 这类兼容消息的归一化，导致上游只收到 `function_call(call_id)`，收不到匹配的 `function_call_output(call_id)`。
  - Supports: 日志里的 tool result 内容已经存在；`ConvertOpenAIRequestToCodex` 的 role 分支只识别显式 `tool`。
  - Conflicts: 无。
  - Test: 在 translator 进入主循环前做一次顺序扫描，把命中的 user 文本改写成 `role="tool" + tool_call_id=<call_id>`，再补定向回归测试。
- H2: 应当把修复收口在 Codex Chat Completions translator，避免扩大到 handler 或其它 provider translator。
  - Supports: 当前 400 来自 `/v1/chat/completions` 到 Codex 的请求翻译链；最小改动点就是 `ConvertOpenAIRequestToCodex`。
  - Conflicts: 无。
  - Test: 只修改 `internal/translator/codex/openai/chat-completions/*` 并运行包级测试。
- H3: 归一化需要限定为“更早 assistant.tool_calls 已声明过的 call_id”，这样可以避免把普通用户文本误判成 tool result。
  - Supports: 本次日志里存在真实上游 assistant tool call；顺序扫描可以天然限定“更早声明”的边界。
  - Conflicts: 无。
  - Test: 增加“前缀存在但 call_id 未声明”用例，确保消息继续保留为 user 文本。

### Experiments
- E1: 只读核对日志与 translator 分支。结果：确认 `messages[30]` 当前不会生成 `function_call_output`，支持 H1。
- E2: 只读核对现有测试。结果：确认标准 `role="tool"` 路径已被 `TestToolCallSimple` 覆盖，本轮只需要补兼容归一化回归。

### Root Cause
- Codex Chat Completions translator 缺少对伪装 tool result user 消息的归一化，导致 `function_call_output` 丢失。

### Fix
- 在 `internal/translator/codex/openai/chat-completions/codex_openai_request.go::ConvertOpenAIRequestToCodex` 进入主输入构造前，顺序扫描消息并把 `[Tool result for <call_id>]: ...` 归一化为标准 tool 消息；同时补多行正文、未知 call_id 与顺序保持的回归测试。


## 2026-04-20 reviewer fix：Chat Completions 伪装 tool result 命中窗口收口

### Observations
- `internal/translator/codex/openai/chat-completions/codex_openai_request.go::parsePseudoToolResultMessage` 初版只要求三项条件：`role="user"`、内容前缀匹配 `[Tool result for `、`call_id` 曾在更早 `assistant.tool_calls[].id` 中出现。
- 现场最小复现：先经历一轮 `assistant.tool_calls -> user 伪装 tool result -> assistant 正常回复`，随后用户再次发送 `"[Tool result for call_order]: 这串前缀是日志格式，请帮我解释含义"`。初版实现会把最后这条普通用户消息继续改写成 `function_call_output(call_order)`。
- 这条误命中发生在 `ConvertOpenAIRequestToCodex` 的归一化阶段，因此会直接污染 Codex 输入 turn 顺序。

### Hypotheses
- H1 ROOT: 命中条件需要从“历史上见过 call_id”收口到“当前仍处于待提交 tool output 的 pending window”；只有最近一轮 assistant tool_calls 刚打开、且对应 call_id 尚未消费时，前缀消息才应视为 tool output。
  - Supports: 误命中样例里的 `call_order` 确实历史上出现过，但该轮 tool call 已在更早 turn 消费完成。
  - Conflicts: 无。
  - Test: 引入 pending call set；assistant 发起新 tool_calls 时重建窗口，tool/兼容 tool result 消费后移除对应 call_id，assistant 新文本回复与普通 user 新提问都会关闭窗口。
- H2: “未知 call_id 保留 user 文本”与“窗口外旧 call_id 保留 user 文本”需要分别覆盖，避免只修一半边界。
  - Supports: 两类误判边界来源不同；一类是未声明 id，一类是旧 id 已过期。
  - Conflicts: 无。
  - Test: 分别保留 `call_missing` 与 `call_order` 旧前缀回归测试。

### Experiments
- E1: 只读检查初版 `normalizePseudoToolResultMessages`。结果：确认它只维护 `seenCallIDs`，没有“待消费窗口”概念，支持 H1。
- E2: 新增“窗口外旧前缀”回归测试。结果：初版失败，收口到 pending window 后通过。

### Root Cause
- 初版兼容逻辑把“历史见过 call_id”当成充分条件，导致窗口外的普通用户引用文本被误改写成 tool output。

### Fix
- `internal/translator/codex/openai/chat-completions/codex_openai_request.go::normalizePseudoToolResultMessages` 现改为维护 pending call window：assistant 新 tool_calls 会重建窗口，tool 或兼容 tool result 会消费对应 call_id，assistant 正常回复与普通 user 新提问会关闭窗口；只有窗口内前缀消息才会归一化。


## 2026-04-20 reviewer fix：撤回 RT 失败 priority=5 写回方案

### Observations
- `sdk/cliproxy/auth/types.go::Auth` 明确把 `Attributes` 标注为 provider immutable configuration。
- `internal/api/handlers/management/auth_files.go::PatchAuthFileFields` 中的 priority 更新属于显式管理操作，会同时写 `Metadata["priority"]` 与 `Attributes["priority"]` 并最终落盘。
- 初版方案在 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的任意 refresh 错误路径里写回 `priority=5` 并触发异步持久化，这会把一次运行期失败扩散成持久化配置变更。

### Hypotheses
- H1 ROOT: refresh 失败路径只应维护运行态错误、退避与 pending 标记，不应覆盖 operator 配置的 priority。
  - Supports: `executeRefreshAuth` 当前本来只管理 `LastError`、`NextRefreshAfter` 与 pending 清理；priority 属于调度拓扑配置。
  - Conflicts: 无。
  - Test: 撤回 priority 写回 helper 与对应断言，恢复“transient failure 不额外落盘 priority 变更”的测试期望。

### Experiments
- E1: 复核 `sdk/cliproxy/auth/types.go::Auth` 与 `internal/api/handlers/management/auth_files.go::PatchAuthFileFields`。结果：确认 priority 属于显式配置面，支持 H1。
- E2: 撤回 `executeRefreshAuth` 中的 priority 写回与测试断言。结果：定向 refresh failure 测试恢复通过。

### Root Cause
- 初版方案把 refresh 失败的运行态处理与 operator priority 配置混在了一起。

### Fix
- 已从 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 删除 priority=5 写回逻辑，并把 `sdk/cliproxy/auth/conductor_refresh_failure_test.go` 恢复为“运行态 refresh 失败不追加 priority 落盘变更”的断言。


## 2026-04-20 Codex free 429 跨模型共享

### Observations
- `sdk/cliproxy/auth/conductor.go::MarkResult` 在 429 时把冷却写进 `auth.ModelStates[result.Model]`，并通过 `updateAggregatedAvailability(...)` 聚合到 auth 级 `Quota`。
- `sdk/cliproxy/auth/selector.go::isAuthBlockedForModel` 在带模型请求时优先只看当前模型对应的 `ModelStates[model]`，因此 `gpt-5.4` 的 429 不会拦住同一 free token 的 `gpt-5.3-codex`。
- 用户目标语义是“Codex free 账号的 429 属于 token 级共享状态”，当前实现与目标存在偏差。

### Hypotheses
- H1（ROOT）：问题根因在 `isAuthBlockedForModel(...)` 只按模型态判定可用性，没有把 free Codex 的 auth 级 quota 冷却共享到兄弟模型。
- H2：`updateAggregatedAvailability(...)` 只在所有模型都 unavailable 时才把 auth 标成 unavailable，因此 free 账号的 auth 聚合态也没有体现跨模型共享冷却。
- H3：`sdk/cliproxy/auth/scheduler.go::projectAggregatedAuthState` 与运行态聚合逻辑保持同样的“全模型都不可用才阻断”规则，因此 scheduler 快路径也会延续这个偏差。

### Experiments
- E1：只读核对 `selector.go`、`conductor.go`、`scheduler.go` 三处代码路径。结果：确认 H1、H2、H3 同时成立。
- E2：设计最小修复为“新增 free Codex shared-quota helper，并在 selector + 两处聚合逻辑复用”。结果：可以把共享语义收口到单一判据。

### Root Cause
- 根因是 free Codex 的 429 语义属于 token 级共享额度，而当前调度与聚合代码沿用了通用的按模型冷却规则。

### Follow-up
- reviewer 补充指出共享冷却还需要继续同步到 `internal/registry/model_registry.go` 暴露层与 `sdk/cliproxy/auth/session_affinity.go` 绑定层；否则会出现“兄弟模型在 `/v1/models` 里仍可见”以及“旧 session 绑定持续先命中冷却 auth 再 fallback”的双重分裂。
- 已在 `sdk/cliproxy/auth/conductor.go::MarkResult` 增加 free Codex shared quota 的全模型联动：429 时批量 `SetModelQuotaExceeded` / `SuspendClientModel`，成功时批量 `ClearModelQuotaExceeded` / `ResumeClientModel`。
- 已新增 `sdk/cliproxy/auth/session_affinity.go::invalidateSessionAffinityAuth`，并在 shared quota 生效时立刻清掉旧 auth 绑定。

## 2026-04-20 Responses misroute reasoning 兼容回归

### Observations
- reviewer 指出的链路是 `sdk/api/handlers/openai/openai_handlers.go::ChatCompletions` → `internal/translator/openai/openai/responses/openai_openai-responses_request.go::ConvertOpenAIResponsesRequestToOpenAIChatCompletions` → `internal/translator/codex/openai/chat-completions/codex_openai_request.go::ConvertOpenAIRequestToCodex`。
- 当前 Codex chat/responses translator 已分别兼容 `reasoning="xhigh"` 与 `reasoning_effort="xhigh"`，定向测试已覆盖直连 translator。
- 中间层 `ConvertOpenAIResponsesRequestToOpenAIChatCompletions` 初始实现只搬 `reasoning.effort`；误投请求若只带字符串 `reasoning` 或顶层 `reasoning_effort`，到 Chat payload 这一跳就会丢档位，后续 Codex chat translator 回落成默认 `medium`。
- 首版 handler 回归测试曾直接检查 executor 捕获到的 payload 上 `reasoning.effort`，实际拿到的是 Chat handler 传给 executor 的 OpenAI Chat 中间态，字段形态仍是 `reasoning_effort`，这条断言层级错误。

### Hypotheses
- H1（ROOT）：根因在 `ConvertOpenAIResponsesRequestToOpenAIChatCompletions` 漏搬 `reasoning_effort` 与字符串 `reasoning`，导致误投 Responses 请求在中间层丢失兼容字段。
- H2：handler 回归测试失败来自断言层级错误；真实 handler 输出仍是 OpenAI Chat payload，需要再补一层 OpenAI Chat -> Codex 翻译后才能断言最终 `reasoning.effort`。

### Experiments
- E1：在 `internal/translator/openai/openai/responses/openai_openai-responses_request.go` 新增 `resolveCompatibleReasoningEffortForResponsesMisroute(...)`，按 `reasoning.effort` → `reasoning_effort` → 字符串 `reasoning` 的顺序保留档位。结果：`timeout 60s go test ./internal/translator/openai/openai/responses -run 'Reasoning|ConvertOpenAIResponsesRequestToOpenAIChatCompletions' -count=1` 通过。
- E2：把 `sdk/api/handlers/openai/openai_handlers_reasoning_compat_test.go` 改成真实经过 `ChatCompletions` handler，再在 capture executor 里调用 `sdk/translator.TranslateRequest(openai -> codex)`，最后断言最终 Codex payload 的 `reasoning.effort`。结果：`timeout 60s go test ./sdk/api/handlers/openai -run 'ChatCompletionsResponsesMisroutePreservesCompatibleReasoning|Compact' -count=1` 通过。
- E3：重跑 `timeout 60s go test ./internal/translator/codex/openai/chat-completions -run 'Reasoning|Tool|CallID|Pseudo|FunctionCallOutput' -count=1` 与 `git diff --check`。结果：均通过。

### Root Cause
- 根因是 Responses-format 误投到 `/v1/chat/completions` 时，中间转换层 `ConvertOpenAIResponsesRequestToOpenAIChatCompletions` 没有继续保留字符串 `reasoning` 与顶层 `reasoning_effort`，导致后续 Codex chat translator 看不到兼容字段并回落到默认档位。

### Fix
- `internal/translator/openai/openai/responses/openai_openai-responses_request.go::resolveCompatibleReasoningEffortForResponsesMisroute` 已补齐三种来源的保留逻辑，并通过 `normalizeResponsesMisrouteReasoningEffort(...)` 统一清洗大小写与空白。
- `sdk/api/handlers/openai/openai_handlers_reasoning_compat_test.go::TestChatCompletionsResponsesMisroutePreservesCompatibleReasoning` 现在覆盖 reviewer 点名的完整 handler 分支，并通过真实 `openai -> codex` 翻译断言最终 `reasoning.effort` 仍为 `xhigh/high`。


## 2026-04-21 复用现有 Codex 配额刷新自动解除 stale 429 冷却

### Observations
- 管理中心当前“仅刷新这个凭证的额度”入口是前端 `Cli-Proxy-API-Management-Center/src/components/quota/quotaConfigs.ts::fetchCodexQuota`，它经由 CPA 后端 `internal/api/handlers/management/api_tools.go::APICall` 代理 `GET /backend-api/wham/usage`；仓内没有独立的 quota refresh management 接口。
- `sdk/cliproxy/auth/selector.go::isAuthBlockedForModel` 会同时读取 auth 聚合态与 model state 的 `NextRetryAfter`、`Quota`、`FailureHTTPStatus` 和 `StatusMessage`；只清 auth 级 `next_retry_after` 仍可能被 model state 里的 429 冷却继续挡住。
- `sdk/cliproxy/auth/conductor.go::MarkResult` 对模型级 429 会同时写入 `ModelState.Quota`、`ModelState.NextRetryAfter`、`Auth.Quota`、`Auth.NextRetryAfter`，并联动 registry 的 `SetModelQuotaExceeded` / `SuspendClientModel`；这说明恢复动作也需要同时回收 manager 与 registry 两层状态。
- 管理中心前端 `quotaConfigs.ts::buildCodexQuotaWindows` 当前按 `limit_window_seconds == 604800` 识别主 code 周窗口；缺失时长字段时回退到 `secondary_window -> primary_window` 顺序，展示层剩余额度口径是 `100 - used_percent`。
- 当前仓内没有可直接复用的“只清 quota 429 冷却” manager helper；现有 `resetModelState` / `clearAuthStateOnSuccess` / `syncAggregatedAuthStateFromModelStates` 是分散的内部工具。

### Hypotheses
- H1 ROOT：最小修复点是直接增强 `internal/api/handlers/management/api_tools.go::APICall`，在成功的 Codex `wham/usage` 响应后按 fresh 周额度判定并调用新的 manager helper 清理 429 冷却。
  - Supports: 用户明确要求复用现有“刷新这个凭证的额度”动作；当前这条动作正是 `APICall -> wham/usage` 闭环。
  - Conflicts: 需要补一层 usage payload 解析 helper，并保证只命中 Codex usage 请求。
  - Test: 在 `APICall` 定向测试里伪造 `wham/usage` 成功响应，断言周剩余 > 30% 时 auth/model 429 状态被清掉，<= 30% 时保持原样。
- H2：只在 management handler 内手动改 auth 聚合态字段就够了。
  - Supports: `selector.go::isAuthBlockedForModel` 在 model 为空时确实会看 auth 聚合态。
  - Conflicts: 真实 pick 路径通常携带模型名，会优先落到 model state；registry 里的 quota/suspend 标记也会继续影响 `/v1/models` 可见性。
  - Test: 只改 auth 聚合态后跑 selector/model-state 场景，验证仍会被 model state 挡住。
- H3：可以直接复用 `Manager.Update(...)` 外加若干手动字段赋值，无需单独 helper。
  - Supports: `Update(...)` 已负责 scheduler upsert、persist 与 hook。
  - Conflicts: quota 清理涉及“哪些字段算 quota 痕迹”“如何保留 401/403”“free shared quota 需要恢复哪些兄弟模型”三层语义，散落在 handler 里容易漏边界。
  - Test: 抽一个 manager helper，并补 auth 层定向测试，验证保留非 quota 错误与 free shared quota 恢复。

### Experiments
- E1：只读核对 `sdk/cliproxy/auth/conductor.go::MarkResult`、`sdk/cliproxy/auth/conductor.go::applyAuthFailureState`、`sdk/cliproxy/auth/selector.go::isAuthBlockedForModel`、`internal/registry/model_registry.go::{SetModelQuotaExceeded,ClearModelQuotaExceeded,SuspendClientModel,ResumeClientModel}`。结果：确认 429 冷却涉及 auth/model 双层 manager 状态与 registry 状态。
- E2：只读核对管理中心 `Cli-Proxy-API-Management-Center/src/components/quota/quotaConfigs.ts::buildCodexQuotaWindows`。结果：确认后端应按 `limit_window_seconds == 604800` 识别主 code 周窗口，缺失时长时回退 secondary/primary 顺序。
- E3：生产修复分两层落地：
  - `sdk/cliproxy/auth/quota_recovery.go::ClearAuthQuotaCooldown`：新增单 auth quota/429 runtime state 清理 helper，清理 auth 聚合态与带 429 痕迹的 model state，并同步 registry 的 quota/suspend 标记。
  - `internal/api/handlers/management/api_tools_codex_quota.go::maybeRecoverCodexQuotaCooldown`：在成功的 Codex `wham/usage` 响应后读取周窗口 `used_percent`，当剩余额度 `> 30%` 时调用 manager helper。
- E4：定向测试已补两层：
  - auth 层 `sdk/cliproxy/auth/quota_recovery_test.go`
  - management 层 `internal/api/handlers/management/api_tools_codex_quota_test.go`

### Root Cause
- 根因是当前“刷新这个凭证的额度”链路只负责向上游查询 fresh `wham/usage`，而 CPA 继续选不到这把 Codex 凭证的真实阻断点仍留在本地 runtime state：`sdk/cliproxy/auth/selector.go::isAuthBlockedForModel` 会持续读取 auth/model 上残留的 429 cooldown 与 registry quota/suspend 标记。

### Fix
- 在 `sdk/cliproxy/auth/quota_recovery.go` 新增 `Manager.ClearAuthQuotaCooldown(...)`，把“清理单 auth 的 quota/429 runtime state”收口成可复用 helper：
  - 清空 auth 聚合态上的 `StatusMessage="quota exhausted"`、`FailureHTTPStatus=429`、`Quota`、`NextRetryAfter`、429 `LastError`
  - 仅重置带 quota/429 痕迹的 model state
  - 重新计算聚合态，并在仍有非 quota 模型错误时把代表性错误重新投影回 auth 聚合态
  - 清理 registry 中对应模型的 `QuotaExceededClients` 与 `SuspendedClients`
- 在 `internal/api/handlers/management/api_tools.go::APICall` 成功读取响应体后接入 `maybeRecoverCodexQuotaCooldown(...)`，让现有 Codex 单凭证 quota refresh 闭环直接承担“恢复可用性”的职责。
- 在 `internal/api/handlers/management/api_tools_codex_quota.go` 按管理中心现有口径解析 `wham/usage`：
  - 优先用 `limit_window_seconds == 604800` 识别周窗口
  - 缺失时长时按 `secondary_window -> primary_window` 回退
  - 使用 `100 - used_percent` 计算剩余额度，严格命中 `> 30%`

---

# reviewer follow-up：Codex quota recovery mixed-state、host 作用域与测试串扰修复（2026-04-21）

## Observations

- `sdk/cliproxy/auth/quota_recovery.go::clearQuotaCooldownModelStates` 与 `clearAuthQuotaRuntimeState` 之前只要看到 quota/429 残留字段就会直接重置状态，没有核对“当前活跃失败类型”是否仍然属于 quota。
- `sdk/cliproxy/auth/conductor.go::applyAuthFailureState` 与 `MarkResult` 在 401/402/403/404 分支会更新 `FailureHTTPStatus`、`StatusMessage`、`NextRetryAfter`，同时旧 `Quota` 结构体可能继续保留；这会形成 mixed-state：当前失败是 unauthorized/payment_required，quota 痕迹仍在。
- `sdk/cliproxy/auth/selector.go::isAuthBlockedForModel` 对 401/403 这类状态会以 `blockReasonOther` 继续阻断，所以如果 quota recovery 把这些 mixed-state 也清掉，就会把本应继续阻断的 auth/model 重新放行。
- `internal/api/handlers/management/api_tools_codex_quota.go::isCodexUsageRefreshRequest` 之前只校验 `provider=codex + GET + path=/backend-api/wham/usage`，没有校验 host 与 `Chatgpt-Account-Id`，因此任意 host 的仿真 usage JSON 都可能触发本地 cooldown 恢复。
- `sdk/cliproxy/auth/quota_recovery_test.go` 新增用例之前带 `t.Parallel()`，同时读写 `internal/registry.GetGlobalRegistry()`；包级 `go test ./sdk/cliproxy/auth -count=5` 已稳定复现 `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 抖动，说明 reviewer 第 3 条成立。

## Hypotheses

### H1: mixed-state 误恢复的根因是 quota recovery 依据“残留字段存在”判断，而不是依据“当前活跃失败仍是 quota”判断（ROOT HYPOTHESIS）
- Supports: `clearAuthQuotaRuntimeState` 只检查 `authHasQuotaCooldown(auth)`；`clearQuotaCooldownModelStates` 只检查 `modelStateHasQuotaCooldown(state)`。
- Conflicts: 若当前活跃失败直接来自 `LastError.HTTPStatus=429`，新旧判据都会清理，需通过 mixed unauthorized 场景证明差异。
- Test: 构造 `FailureHTTPStatus/LastError=401 + Quota residue` 的 auth/model，断言 `ClearAuthQuotaCooldown` 返回 unchanged，`isAuthBlockedForModel` 仍保持 `blockReasonOther`。

### H2: APICall 作用域过宽的根因是 recovery helper 只识别 path，没有把官方 host 与凭证身份一起纳入匹配（ROOT HYPOTHESIS）
- Supports: 现有 helper 既不看 `parsedURL.Hostname()`，也不校验 `Chatgpt-Account-Id` 与 auth metadata 的一致性。
- Conflicts: 若 management 前端未来改 header key，当前新判据会需要同步更新。
- Test: 加入 host=`mock.local` 与错误 account_id 的测试，请求成功返回 usage JSON 时也不应清理 cooldown。

### H3: 包级并发抖动的根因是新增测试在 `t.Parallel()` 下修改全局 registry 单例（ROOT HYPOTHESIS）
- Supports: reviewer 已复现 flake；新增测试直接注册/冻结/恢复全局模型目录。
- Conflicts: 包内原有并行测试很多，若新测试改成串行仍需用 `go test -count=5` 再确认。
- Test: 去掉新增 registry 测试的 `t.Parallel()`，重复跑 `go test ./sdk/cliproxy/auth -count=5`。

## Experiments

- 运行 `timeout 60s go test ./sdk/cliproxy/auth -count=5`，稳定复现 `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 抖动，证实新增并行 registry 测试会污染包级共享状态。
- 只读核对 `sdk/cliproxy/auth/conductor.go::{MarkResult,applyAuthFailureState}` 与 `sdk/cliproxy/auth/quota_recovery.go`，确认 401/403 覆盖时会保留旧 quota 结构，旧 helper 会把 mixed-state 误判成可清理 quota。
- 只读核对 `internal/api/handlers/management/api_tools.go::APICall` 与 `api_tools_codex_quota.go::isCodexUsageRefreshRequest`，确认当前恢复副作用仅靠 path 命中，没有 host/account_id 收口。
- 生产修复：
  - 新增 `sdk/cliproxy/auth/quota_recovery_state.go`，把 quota recovery 判据改成“当前活跃失败类型分类”；只有当前 failure kind 仍是 quota 时才清理 auth/model 冷却。
  - 收紧 `internal/api/handlers/management/api_tools_codex_quota.go`，要求同时命中 `GET + host=chatgpt.com + path=/backend-api/wham/usage + Chatgpt-Account-Id 与 auth.Metadata.account_id 一致` 才触发恢复。
  - 去掉新增 quota recovery 测试中的并行执行，并补 mixed-state / host-scope 定向测试。

## Root Cause

- mixed-state 误恢复的根因是 quota recovery 之前按“残留 quota 字段存在”做清理判据，而 401/403 等新失败覆盖后旧 quota 结构仍可能残留，导致当前 unauthorized/payment_required 状态被误清空。
- APICall 作用域过宽的根因是 Codex usage recovery helper 只按 path 命中 `/backend-api/wham/usage`，缺少官方 host 与目标账号身份校验。
- 包级测试抖动的根因是新增测试在 `t.Parallel()` 下读写全局 model registry 单例，和现有 session-affinity / scheduler 并行用例共享同一份运行态。

## Fix

- `sdk/cliproxy/auth/quota_recovery.go` 现已只负责清理流程编排；具体“当前活跃失败是否属于 quota”判据拆到 `sdk/cliproxy/auth/quota_recovery_state.go`：
  - `authCurrentFailureKind(...)`
  - `modelStateCurrentFailureKind(...)`
  - `errorCurrentFailureKind(...)`
  - `failureKindFromStateSignals(...)`
- `clearAuthQuotaRuntimeState(...)` 与 `clearQuotaCooldownModelStates(...)` 现在只有在当前 failure kind 仍是 quota 时才执行清理；401/403/not_found mixed-state 会保持原阻断状态。
- `internal/api/handlers/management/api_tools_codex_quota.go::isCodexUsageRefreshRequest` 现已要求同时满足：
  - `provider=codex`
  - `method=GET`
  - `hostname=chatgpt.com`
  - `path=/backend-api/wham/usage`
  - `Chatgpt-Account-Id` 与 `auth.Metadata.account_id` 一致
- `sdk/cliproxy/auth/quota_recovery_test.go` 与 `internal/api/handlers/management/api_tools_codex_quota_test.go` 已补 mixed unauthorized、非官方 host、缺失/错误 account_id 等回归场景，并移除会写全局 registry 的新测试并行执行。

## 2026-04-21 v6.9.31 四项 port 验证中的包级测试失败排查

### Observations

- 本轮已移植四项：自定义 Host 透传、refresh 成功但仍需刷新时 30 秒退避、Codex streaming completed.output 回填、/healthz HEAD 支持。
- 定向测试通过：`timeout 60s go test ./internal/util ./internal/api ./internal/runtime/executor ./sdk/cliproxy/auth -run 'TestApplyCustomHeadersFromAttrs_MirrorsHostToRequestAndHeaderMap|TestHealthz|TestCodexExecutorExecute(_|Stream_)EmptyStreamCompletionOutputUsesOutputItemDone|TestManagerRefreshAuth_SchedulesBackoffWhenRefreshStillNeeded' -count=1`。
- 包级测试命令 `timeout 60s go test ./internal/util ./internal/api ./internal/runtime/executor ./sdk/cliproxy/auth -count=1` 中，前三个包通过，`./sdk/cliproxy/auth` 失败在 `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth`。
- 失败现象：`session_affinity_test.go:155: Execute() second payload = "auth-a", want "auth-b"`。
- 当前本轮改动触及 `sdk/cliproxy/auth/conductor.go::executeRefreshAuth` 的 refresh 成功收尾逻辑，未触及 session affinity 选择/重绑主链。

### Hypotheses

#### H1: session-affinity 包级失败是当前 HEAD 既有回归（ROOT HYPOTHESIS）
- Supports: 失败点在 `session_affinity_test.go`，与本轮四项 port 的直接代码路径距离较远；定向 refresh 测试已通过。
- Conflicts: 本轮也修改了 `sdk/cliproxy/auth/conductor.go`，同包完整测试失败仍需现场排除副作用。
- Test: 在当前工作区单独运行该测试；再用 clean HEAD worktree 运行同一个测试对照。

#### H2: 本轮 `executeRefreshAuth` 的 `m.shouldRefresh(updated, now)` 调用触发 runtime/scheduler 副作用，间接影响 session affinity
- Supports: 修改位于同一个 Manager 类型。
- Conflicts: 失败测试名称和日志指向普通执行重绑，未看到 refresh 调用链。
- Test: 查看失败测试源码是否注册 refresh executor 或调用 refresh；必要时对比单测运行日志。

#### H3: 包级并发测试泄漏全局 session-affinity 状态导致该测试顺序敏感
- Supports: 完整包测试失败，定向新增测试通过；日志里出现多个 session-affinity cache hit。
- Conflicts: 需要用单测复现和 `-run` 子集确认。
- Test: 单独运行 `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 并重复多次。

### Experiments

- 单独运行 `timeout 60s go test ./sdk/cliproxy/auth -run 'TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth' -count=1 -v`，结果通过；日志显示第二次命中 `auth-b`。
- 重复运行 `timeout 60s go test ./sdk/cliproxy/auth -run 'TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth' -count=20`，结果通过。
- 查看 `sdk/cliproxy/auth/session_affinity_test.go::TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth`：测试使用 provider `gemini`、自定义 `sessionAffinityExecutor`，未调用 `Manager.refreshAuth` 或 Codex refresh 路径。

### Conclusion

- 当前证据支持 H1/H3：包级失败来自 `sdk/cliproxy/auth` 既有并行测试/全局 registry 状态交互；本轮新增 refresh backoff 定向测试和 session-affinity 单测均通过。

### Additional Experiments

- 复跑 `timeout 60s go test ./sdk/cliproxy/auth -count=1`，仍失败在 `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth`，同样为第二次 payload 命中 `auth-a`。
- 查看本轮 `sdk/cliproxy/auth/conductor.go` diff，仅新增 `refreshIneffectiveBackoff` 常量和 `executeRefreshAuth` 成功收尾里的 `m.shouldRefresh(updated, now)` 退避判定。
- 查看 `sdk/cliproxy/auth/session_affinity.go` 与 `sdk/cliproxy/auth/session_affinity_test.go`，当前工作区无本轮 diff。
- 运行 `timeout 60s go test ./sdk/cliproxy/auth -run 'TestManagerRefreshAuth|TestManagerShouldRefresh|TestManagerCollectRefreshTargets|TestManagerRefreshAuthNow|TestManagerExecuteStream_Codex401|TestManagerExecute_Codex401|TestSchedulerPick|TestManager_PickNextMixed' -count=1` 通过。

### Root Cause Boundary

- 本轮 port 引入的 refresh backoff 改动已由定向测试覆盖，并且相关 refresh / scheduler pick 子集通过。
- `TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 的包级失败属于 session-affinity 并行测试与全局 registry/scheduler 状态交互风险，当前回合按用户确认的 v6.9.31 四项 port 不扩大修复范围。

## 2026-04-21 reviewer P1：auth 包级失败修复排查

### Observations

- 当前工作区运行 `timeout 60s go test ./sdk/cliproxy/auth -count=1` 可复现失败。
- 本次复现出现两个失败：
  - `sdk/cliproxy/auth/scheduler_test.go::TestManager_SchedulerTracksMarkResultCooldownAndRecovery`，断言 `len(seen) = 1, want 2`。
  - `sdk/cliproxy/auth/session_affinity_test.go::TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth`，本次表现为 first execute `auth_not_found`。
- 使用 `git worktree add --detach <tmp> HEAD` 建立 clean HEAD，对照运行 `timeout 60s go test ./sdk/cliproxy/auth -count=1` 通过。
- 因此当前工作区变更确实改变了包级测试结果，需要在本轮修复。

### Hypotheses

#### H1: 新增 refresh ineffective backoff 测试污染全局 registry 中通用 auth ID（ROOT HYPOTHESIS）
- Supports: 包级失败集中在 scheduler/session-affinity，二者都依赖全局 registry 的 auth->model 支持关系；新增测试使用 auth ID `refresh-ineffective`，但不直接注册 registry，支持力度有限。
- Conflicts: 失败 auth 多为 `auth-a`/`auth-b`，与新增测试 auth ID 不同。
- Test: 单独运行新增测试与失败测试组合，观察是否复现。

#### H2: 本轮 `executeRefreshAuth` 成功后调用 `m.shouldRefresh(updated, now)` 触发 Update/scheduler upsert 后，改变 scheduler cursor 或状态，影响后续并行测试
- Supports: 新逻辑位于 `executeRefreshAuth`，会在成功 refresh 后按 `shouldRefresh` 写入 `NextRefreshAfter`，随后 `m.Update` 会 upsert scheduler。
- Conflicts: 各测试创建独立 manager；全局共享主要是 registry。
- Test: 组合运行 refresh 相关测试和 scheduler/session-affinity 测试；查看失败是否随 refresh 子集出现。

#### H3: 上游四项移植新增的测试文件或包级并发顺序放大了既有测试中全局 registry auth ID 冲突
- Supports: clean HEAD 通过，当前工作区新增/修改测试会改变并行调度；失败测试使用 `auth-a`/`auth-b` 这类通用 ID，其他测试也大量复用这些 ID。
- Conflicts: 单独运行失败测试通过。
- Test: 搜索通用 auth ID 的 registry 注册，检查是否存在并行测试对相同 auth ID 注册不同模型后 Cleanup 互删。

### Experiments

- 组合运行 `timeout 60s go test ./sdk/cliproxy/auth -run 'TestManagerRefreshAuth_SchedulesBackoffWhenRefreshStillNeeded|TestManager_SchedulerTracksMarkResultCooldownAndRecovery|TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth' -count=20`，修复前稳定失败。
- 搜索发现并行测试中复用全局 registry 的通用 auth ID：
  - `sdk/cliproxy/auth/scheduler_test.go::TestManager_SchedulerTracksMarkResultCooldownAndRecovery` 使用 `auth-a` / `auth-b` 直接对 `registry.GetGlobalRegistry()` 注册模型。
  - `sdk/cliproxy/auth/session_affinity_test.go::TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 通过 `registerSchedulerModels(...)` 也对同一全局 registry 注册 `auth-a` / `auth-b`。
- clean HEAD 通过、当前工作区失败，说明本轮新增并行测试调度把既有全局 ID 冲突放大成稳定失败。
- 把两个测试改成唯一 auth ID 后，上述 `-count=20` 组合测试通过。

### Root Cause

- `sdk/cliproxy/auth` 包内并行测试共享 `registry.GetGlobalRegistry()`，两个测试复用了相同的 `auth-a` / `auth-b` clientID，Cleanup 互删对方注册，导致 scheduler/session-affinity 随机看到缺失模型映射。

### Fix

- `sdk/cliproxy/auth/session_affinity_test.go::TestManagerExecute_SessionAffinityRebindsToSuccessfulFallbackAuth` 改用唯一 auth ID：`session-affinity-rebind-a/b`。
- `sdk/cliproxy/auth/scheduler_test.go::TestManager_SchedulerTracksMarkResultCooldownAndRecovery` 改用唯一 auth ID：`scheduler-cooldown-recovery-a/b`。

## 2026-04-22 reviewer：management usage 新字段回归修复

### Observations

- reviewer 指出 `first_token_ms` 只依赖 Gin context 的 `API_RESPONSE_TIMESTAMP`。
- 现场代码确认：`sdk/api/handlers/stream_forwarder.go::BaseAPIHandler.ForwardStream` 在 `writeChunk(chunk)` 前后都没有设置 `API_RESPONSE_TIMESTAMP`。
- 现场代码确认：`sdk/api/handlers/handlers.go::GetContextWithCancel` 里的 `appendAPIResponse(c, payload)` 受 `h.Cfg.RequestLog` 条件影响；默认 `request-log=false` 时，主流 SSE 不会通过这条路径写入时间戳。
- 现场代码确认：`internal/usage/logger_plugin.go::MergeSnapshot` 对重复明细直接 `Skipped++`，当前不会把新快照里的 `request_type`、`first_token_ms`、`user_agent` 回填到旧明细。

### Hypotheses

#### H1: 主流 SSE 首包时间缺失来自 `ForwardStream` 没有在首个数据 chunk 写入 `API_RESPONSE_TIMESTAMP`（ROOT HYPOTHESIS）
- Supports: OpenAI / Responses / Claude / Gemini 的 SSE handler 都调用 `BaseAPIHandler.ForwardStream`；该函数当前只写 chunk、flush、keepalive、terminal error，没有写时间戳。
- Conflicts: websocket 路径已有 `markAPIResponseTimestamp`，因此该问题集中在 SSE 转发主链。
- Test: 给 `stream_forwarder_test.go` 增加真实 `ForwardStream` 用例，断言第一条数据 chunk 后 Gin context 出现 `API_RESPONSE_TIMESTAMP`。

#### H2: `first_token_ms=0` 来自 usage 记录发生在首包之前
- Supports: usage 发布点位于 executor 流结束或 usage 事件解析处，存在先后顺序风险。
- Conflicts: reviewer 指向的主因是上下文时间戳缺失；只要 `ForwardStream` 首 chunk 写入时间戳，流结束后的 usage 发布可读取到该值。
- Test: 补 `ForwardStream` 时间戳测试后，再跑 usage 层 `RequestStatistics` 用例确认读取逻辑保持有效。

#### H3: 历史快照补齐失败来自 `dedupKey` 排除了新增观测字段且重复命中没有 merge 元数据
- Supports: `dedupKey` 只包含时间戳、source、client_ip、auth_index、failed 与 token；`MergeSnapshot` 看到重复 key 后直接跳过。
- Conflicts: 排除观测字段是保留导入去重稳定性的设计，修复点应落在重复命中后的元数据补齐，而不是扩大 dedup key。
- Test: 增加先导入旧明细、再导入同 key 新明细的测试，断言现有 detail 补齐三项元数据且总请求数保持不变。

### Experiments

- 代码阅读确认 H1：`ForwardStream` 数据分支当前只有 `writeChunk(chunk)`、`pendingBytes += len(chunk)`、`flushPending(false)`，没有 `API_RESPONSE_TIMESTAMP` 写入点。
- 代码阅读确认 H3：`MergeSnapshot` 的 `seen` 只有 `map[string]struct{}`，重复时没有拿到现有 detail 引用，因此无法回填新增观测字段。

### Root Cause

- SSE 主链没有在 `ForwardStream` 首个真实数据 chunk 处写入 `API_RESPONSE_TIMESTAMP`，导致默认 `request-log=false` 时 `resolveFirstTokenMs` 读取不到首包时间。
- `MergeSnapshot` 的重复明细处理只做跳过计数，没有对旧快照空元数据执行补齐。

### Fix Plan

- 在 `sdk/api/handlers` 增加共享 `markAPIResponseTimestamp` helper，`appendAPIResponse` 与 `ForwardStream` 都复用它；`ForwardStream` 在首个非空数据 chunk 写入时间戳。
- 把 `MergeSnapshot` 的 seen map 改为指向现有 detail 的引用，重复命中时仅补齐空的 `request_type`、`first_token_ms`、`user_agent`。
- 增加 `ForwardStream` 首包时间测试与 `MergeSnapshot` 元数据回填测试。

### Fix Results

- 在 `sdk/api/handlers/handlers.go::markAPIResponseTimestamp` 抽出共享时间戳 helper，并让 `appendAPIResponse` 复用它。
- 在 `sdk/api/handlers/stream_forwarder.go::BaseAPIHandler.ForwardStream` 的首个非空数据 chunk 写入前调用 `markAPIResponseTimestamp(c)`。
- 在 `internal/usage/logger_plugin.go::MergeSnapshot` 中把 `seen` 改为现有 detail 引用；重复命中时通过 `mergeRequestDetailMetadata` 仅补齐空的 `request_type`、`first_token_ms`、`user_agent`。
- 新增 `sdk/api/handlers/stream_forwarder_test.go::TestForwardStreamMarksAPIResponseTimestampOnFirstChunk` 覆盖默认 SSE 转发首包时间戳。
- 新增 `internal/usage/logger_plugin_test.go::TestRequestStatisticsMergeSnapshotBackfillsDuplicateUsageMetadata` 覆盖历史快照元数据回填。

### Verification

- `timeout 60s go test ./sdk/api/handlers -run 'ForwardStream|APIResponse' -count=1` 通过。
- `timeout 60s go test ./internal/usage -run 'MergeSnapshot|RequestStatisticsRecordIncludesStreamMetadata|ClassifyRequestType' -count=1` 通过。
- `timeout 60s go test ./internal/usage ./internal/api/handlers/management ./internal/logging ./internal/api/middleware ./sdk/api/handlers -timeout 60s` 通过。
- `timeout 60s go test ./... -run '^$'` 通过。
- `git diff --check` 通过。

## 2026-04-23 reviewer：Codex free 共享阻断范围过宽

### Observations

- reviewer 指出 `sdk/cliproxy/auth/codex_quota_scope.go::codexFreeSharedBlockFromModelState` 当前只看 `Unavailable + NextRetryAfter`，会把 `model_not_supported`、普通 `404`、`5xx/408` 都当成 free token 级共享阻断。
- 现场代码确认：`sdk/cliproxy/auth/conductor.go::MarkResult` 在 `isModelSupportResultError(...)`、`case 404`、`case 408/500/502/503/504` 分支都会先写当前模型的 `NextRetryAfter`，随后统一调用 `codexFreeSharedBlockFromModelState(...)` 决定是否镜像到全 token。
- 现场代码确认：当前 `codexFreeSharedBlockFromAuthState(...)` 也只看 `auth.Unavailable + auth.NextRetryAfter`，因此即便聚合态来自非账号级错误，也可能被 selector 提前当成共享阻断。

### Hypotheses

#### H1: 共享阻断 helper 没有检查错误类型，导致所有未来不可用状态都被扩散（ROOT HYPOTHESIS）
- Supports: helper 当前没有用 `FailureHTTPStatus` / `LastError` 做 token-scope 分类。
- Conflicts: 无。
- Test: 把 helper 收窄到 `401/402/403/429`，再补 `model_not_supported`、`404`、`503` 负向测试，验证 sibling model 不再受影响。

#### H2: 问题只在 `MarkResult` 的 mirror 调用点，helper 本身没问题
- Supports: mirror 是实际把状态扩散到所有模型的最后一步。
- Conflicts: selector/scheduler 也直接消费 helper；即便不 mirror，聚合态 helper 仍可能误判共享阻断。
- Test: 同时检查 `codexFreeSharedBlockFromAuthState(...)` 与 `codexFreeSharedBlockFromModelState(...)`。

#### H3: 产品语义其实就是“Codex free 任意模型失败都扩散全 token”
- Supports: 之前实现确实沿这个方向扩了。
- Conflicts: 既有 memory 和本轮 reviewer 都把共享边界收口到账号级错误；现有测试也只覆盖 `401/429`，没有覆盖 `model_not_supported/404/5xx`。
- Test: 若真是预期，应补覆盖三类场景；当前先按 reviewer 合理边界收窄实现。

### Experiments

- 代码阅读确认 H1/H2：`codexFreeSharedBlockFromModelState(...)` 当前只要 `state.Unavailable && state.NextRetryAfter.After(now)` 就返回 blocked；`codexFreeSharedBlockFromAuthState(...)` 对聚合态也同样宽。
- 将 helper 收窄为只认 `401/402/403/429`：
  - auth 聚合态改为依赖 `currentPersistableFailureHTTPStatus(auth)`。
  - model state 改为依赖 `currentPersistableModelFailureHTTPStatus(state)`。
  - 保留 quota 专项分支，但移除 `StatusDisabled` 的自动共享扩散。
- 新增负向回归测试：
  - `TestCodexFreeSharedBlockFromModelState_OnlyAccountScopedStatusesShare`
  - `TestManagerMarkResult_CodexFreeModelScopedFailuresDoNotHideSiblingModels`
  - `TestManagerMarkResult_CodexFreeHTTP503DoesNotBlockSiblingModel`

### Root Cause

- Codex free 共享阻断 helper 缺少账号级错误分类，错误地把所有“未来不可用”的模型状态都提升成 token 级共享阻断。

### Fix Results

- `sdk/cliproxy/auth/codex_quota_scope.go` 现在只把 `401/402/403/429` 识别为 Codex free token 级共享错误；`model_not_supported`、普通 `404`、`5xx/408` 继续只影响当前模型。
- `selector.go::isAuthBlockedForModel` 与 `scheduler.go::projectAggregatedAuthState` 通过收窄后的 helper 自动继承新边界，无需额外分支。
- `conductor.go::MarkResult` 保留共享镜像调用点，但只有命中账号级错误时才会镜像到 sibling models；模型能力错误与瞬时错误不再误扩散。

### Verification

- `timeout 60s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'CodexFree|ExcludedModels' -count=1` 通过。
- `timeout 60s go test ./sdk/cliproxy ./sdk/cliproxy/auth ./internal/registry -count=1` 通过。
- `timeout 60s go test ./... -run '^$'` 通过。
- `git diff --check` 通过。

## 2026-04-23 Codex free：fill-first 队列顺序统一

### Observations

- 用户进一步确认：既然 Codex free 已经统一成“能力面不区分模型、账号级错误共享”，那 fill-first 下 free token 的请求顺序也应该一致。
- 代码复核确认，`sdk/cliproxy/auth/selector.go::FillFirstSelector.Pick` 当前是 `getAvailableAuths(...) -> sortAuthsByFirstRegisteredAt(...) -> 取第一个`；它对“当前模型可用集合”排序后再选头部。
- 代码复核确认，`sdk/cliproxy/auth/scheduler.go::pickSingle` 仍按 `providerState.ensureModelLocked(modelKey, ...)` 走 per-model shard，但 shard 内部实际通过 `readyView.pickFirstRegistered(...)` 选出首次入池时间最早的 ready auth。
- 这意味着 scheduler 侧原本就更接近“稳定顺序 + 当前模型局部跳过”；selector 侧语义虽然结果通常等价，但没有把这层意图显式写出来。

### Hypotheses

#### H1: 真正需要修的是 selector 的表达方式，而不是 scheduler 的整体算法（ROOT HYPOTHESIS）
- Supports: scheduler 已经通过 `pickFirstRegistered(...)` 保持稳定顺序；selector 仍是先过滤可用集合再排序。
- Conflicts: 如果 per-model shard 真会改变 fill-first 的相对顺序，那 scheduler 也得跟着改。
- Test: 把 selector 改成“扫描稳定顺序并局部跳过当前模型不可用 auth”，再补 selector/scheduler 两侧回归测试验证跨模型结果。

#### H2: scheduler 也需要改成 provider 级共享队列，否则不同模型还是会乱序
- Supports: `pickSingle(...)` 入口仍然拿的是 `modelKey` shard。
- Conflicts: shard 内选择器用的是 `pickFirstRegistered(...)`，只要静态模型集一致、模型级错误只做局部 skip，结果已经等价于共享顺序。
- Test: 用同一组 Codex free auth 对两个模型分别 `pickSingle(...)`，看是否表现为“老账号在当前模型失败时被局部跳过、其它模型仍优先老账号”。

#### H3: 其实无需代码改动，只补测试就够了
- Supports: 经过代码阅读，scheduler 的现状已经满足目标语义。
- Conflicts: selector 侧没有显式把这条语义钉死，后续改动容易再次退回“按当前模型重排可用集合”的思路。
- Test: 若只补测试，不改 selector helper，则语义仍然主要靠读代码推断；本轮先做最小 helper 收口。

### Experiments

- 先做只读核对：确认 `readyView.pickFirstRegistered(...)` 已经按 `firstRegisteredAtLess(...)` 比较，不依赖 `auth.ID` 顺序；因此 scheduler 侧不需要额外重写全局队列。
- 新增 `sdk/cliproxy/auth/fill_first_selection.go`：把 fill-first 选择显式收口为“稳定队列上的首个可用账号 + 冷却摘要”，模型级失败仅对当前请求局部跳过。
- `sdk/cliproxy/auth/selector.go::FillFirstSelector.Pick` 改为复用 `scanFillFirstCandidates(...)`，不再通过“取当前模型可用切片再排序”来隐式表达语义。
- 新增 `sdk/cliproxy/auth/codex_free_fill_first_order_test.go`：
  - `TestFillFirstSelectorPick_CodexFreeKeepsSharedOrderAcrossModels`
  - `TestSchedulerPickSingle_CodexFreeKeepsSharedOrderAcrossModels`
- 首次实验发现新增 scheduler 测试失败为 `auth_not_found`；复核后确认不是生产逻辑问题，而是测试基座 `scheduler_test.go::registerSchedulerModels` 每次只注册一个模型，第二次注册覆盖了第一组模型。
- 将测试改为本地 `registerSchedulerModelSet(...)` 一次注册同一 auth 的两个模型后，selector/scheduler 两侧回归均通过。

### Root Cause

- 问题不在 scheduler 的实际 fill-first 选择算法，而在 selector 没有把“稳定顺序 + 当前模型局部跳过”的 Codex free 语义显式写死，缺少直接回归测试时容易被误判成“不同模型队列顺序不一致”。

### Fix Results

- `sdk/cliproxy/auth/fill_first_selection.go::scanFillFirstCandidates` 新增统一 helper，明确 fill-first 按稳定顺序挑选，而不是按当前模型重排整个队列。
- `sdk/cliproxy/auth/selector.go::{shouldPreferCodexWebsocket,FillFirstSelector.Pick}` 现在直接复用该 helper；Codex websocket 优先仍保留在同一优先级桶内生效。
- `sdk/cliproxy/auth/codex_free_fill_first_order_test.go` 锁定了目标语义：
  - 老 free token 在模型 A 上命中模型级 404 时，模型 A 请求会局部跳到下一个 token；
  - 但模型 B 请求仍然会优先选择这枚更早入池的 token。
- scheduler 生产代码本轮未改：代码级核对与回归测试都表明它已经满足相同语义。

### Verification

- `timeout 60s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'CodexFree|ExcludedModels|FillFirst' -count=1` 通过。
- `timeout 60s go test ./sdk/cliproxy ./sdk/cliproxy/auth ./internal/registry -count=1` 通过。
- `timeout 60s go test ./... -run '^$'` 通过。
- `git diff --check` 通过。

## 2026-04-23 reviewer follow-up：Codex free shared success 清理过宽与 thinking suffix 残留

### Observations

- reviewer 指出 `sdk/cliproxy/auth/conductor.go::MarkResult` 的 success 分支当前只要命中 `codexFreeSharesModelState(auth)`，就会调用 `resetCodexFreeSharedModelStates(...)` 清空整枚 free token 的全部 runtime states，并对全部模型统一 `ResumeClientModel(...)`。
- 代码复核确认：这个 helper 之前依赖 `sdk/cliproxy/auth/codex_quota_scope.go::codexFreeSharedRuntimeModelIDs(...)`，它会把 registry model IDs 与 `Auth.ModelStates` 统一 canonical 到 base model，再批量 reset / resume。
- 这会产生两个错误：
  - 模型 A 的 `404 / model_not_supported / 503` 会在模型 B success 时被误恢复；
  - `gpt-5.4(high)` 这类 exact suffix key 不会被 shared success 清掉，因为 shared helper 最终只处理 base key。
- 现场代码还确认：`sdk/cliproxy/auth/selector.go::isAuthBlockedForModel` 先查 exact key、再回退 base key，因此 stale suffix state 只要残留，就会一直拦住 exact suffix 请求。

### Hypotheses

#### H1: success 路径把“shared clear”与“模型家族当前请求恢复”混成一套全量 reset，导致无关模型级失败被误清（ROOT HYPOTHESIS）
- Supports: `resetCodexFreeSharedModelStates(...)` 直接遍历全部 shared runtime model IDs。
- Conflicts: 无。
- Test: 拆成“当前请求模型家族 reset”与“仅 token-scoped states shared clear”，再补 sibling model-specific failure 回归。

#### H2: suffix 残留的根因是 shared runtime helper 先做 canonical 去重，exact key 从未被清理
- Supports: 原 helper 对 `Auth.ModelStates` key 调 `canonicalModelKey(...)` 后再 dedupe；selector 又优先查 exact key。
- Conflicts: 无。
- Test: 改成运行态 key 保留 exact/suffix，不再跟 registry model IDs 共用同一套 helper；补 success 后 exact suffix 恢复回归。

#### H3: 只修 success reset 还不够，failure 侧 shared mirror 继续把 source state 覆盖到所有兄弟模型，也会让恢复边界失真
- Supports: 原 `mirrorCodexFreeSharedModelState(...)` 会把当前 source state clone 到全部模型；这会让 shared failure 与模型级 failure 混在同一批 runtime states 里。
- Conflicts: 若只靠 auth 聚合态/shared helper 就足够阻断兄弟模型，则 mirror 不再必要。
- Test: 删掉 shared mirror，仅保留 auth 聚合态 + registry/session-affinity 联动，再跑现有 shared runtime / quota 回归。

### Experiments

- 先按 reviewer 提示做代码级复核，确认 P1/P2 都成立：
  - success 确实会无差别清空全部 shared runtime model IDs；
  - shared runtime helper 确实会把 suffix key canonical 掉，导致 exact key 清不掉。
- 新增 `sdk/cliproxy/auth/model_state_scope.go`，把两类概念拆开：
  - `modelStateFamilyKeys(...)` 只处理运行态 state key，保留 exact/base/已有同家族 suffix；
  - `registryModelFamilyIDs(...)` 只处理 registry 真实 model IDs。
- 修改 `sdk/cliproxy/auth/conductor.go::MarkResult`：
  - success 先清“当前请求模型家族”的 runtime keys；
  - Codex free 若存在 token-scoped shared block，再只清 token-scoped runtime keys，并在批量 `Resume` 之后根据剩余 model states 重新挂回仍应保留的模型级 suspend/quota；
  - failure 不再 `mirrorCodexFreeSharedModelState(...)`，只更新当前模型家族状态，shared block 继续通过 auth 聚合态生效。
- 新增回归测试：
  - `TestManagerMarkResult_CodexFreeSuccessKeepsSiblingModelSpecificFailure`
  - `TestManagerMarkResult_CodexFreeSharedSuccessReappliesSiblingModelSpecificFailure`
  - `TestManagerMarkResult_CodexFreeSharedSuccessClearsThinkingSuffixState`

### Root Cause

- Codex free shared success 之前错误地复用了“全 token runtime model IDs”做统一 reset/resume，把 token-scoped shared clear、模型级 failure 保留，以及 exact suffix key 清理三个语义混成了一层处理。

### Fix Results

- `sdk/cliproxy/auth/model_state_scope.go` 新增了运行态模型家族 key、registry model family IDs、token-scoped runtime keys 与剩余 registry block 重建 helper，明确分离“state key”与“registry model ID”。
- `sdk/cliproxy/auth/conductor.go::MarkResult` 的 success 路径现在只会：
  - 清当前请求模型家族；
  - 若存在 shared block，再额外清 token-scoped keys；
  - shared clear 批量恢复 registry 后，会把仍然有效的模型级 `404 / model_not_supported` 等状态重新挂回 registry。
- `sdk/cliproxy/auth/conductor.go::MarkResult` 的 failure 路径不再把 shared failure clone 到全部兄弟模型；兄弟模型阻断继续由 auth 聚合态与 `codexFreeSharedBlockForAuth(...)` 提供。
- `sdk/cliproxy/auth/codex_quota_scope.go` 删除了旧的 `codexFreeSharedRuntimeModelIDs(...)`，避免后续再把 runtime key 与 registry model IDs 混用。

### Verification

- `timeout 60s go test ./sdk/cliproxy/auth -run 'CodexFree|ThinkingSuffix' -count=1` 通过。
- `timeout 60s go test ./sdk/cliproxy/auth ./sdk/cliproxy -run 'CodexFree|ExcludedModels|FillFirst' -count=1` 通过。
- `timeout 60s go test ./sdk/cliproxy ./sdk/cliproxy/auth ./internal/registry -count=1` 通过。
- `timeout 60s go test ./... -run '^$'` 通过。
- `git diff --check` 通过。

----

# 2026-04-24 review follow-up 调试记录

## Observations
- review 指出 alias 路由 `/backend-api/codex/responses*` 已注册，但 `internal/api/middleware/request_logging.go::isResponsesWebsocketUpgrade` 只识别 `/v1/responses`，因此 GET websocket alias 不会进入现有 request logging 链路。
- 当前 `internal/logging/gin_logger.go::aiAPIPrefixes` 已补 `/v1/images`，但未补 `/backend-api/codex`，所以 alias POST 请求不会生成 request_id，主日志与 request log 无法串联。
- 当前 `internal/runtime/executor/codex_request_plan.go::{normalizeCodexPreparedBody,normalizeCodexPreparedBodyFallback}` 会无条件调用 `internal/runtime/executor/codex_image_tool.go::ensureCodexImageGenerationTool`，唯一跳过条件是 model 后缀 `spark`。
- 需要进一步核对图片入口如何构造 Codex request，确认 image tool 注入是否应当只在显式图片语义请求上触发，而不是所有普通文本请求。

## Hypotheses
### H1: alias 日志断链的根因是路径白名单没有同步扩到 `/backend-api/codex/responses`（ROOT HYPOTHESIS）
- Supports: `isResponsesWebsocketUpgrade` 对路径做精确等值判断；`aiAPIPrefixes` 也只覆盖 `/v1/*` 与 `/api/provider/*`。
- Conflicts: 暂无。
- Test: 仅扩展路径识别并补定向测试，验证 alias GET/POST 都被判定为 Responses/AI API 路径。

### H2: 图片工具注入过宽的根因是当前 helper 只按模型后缀过滤，没有读请求本身的图片意图
- Supports: `ensureCodexImageGenerationTool` 只接受 `body, baseModel` 两个参数；任何普通文本 request 进入 `normalizeCodexPreparedBody` 都会被扩展工具集。
- Conflicts: 上游 commit 标题是“for all Codex upstream requests”，需要结合本仓当前 `/v1/images` 专门桥接实现重新收口本地边界。
- Test: 查看 `openai_images_build.go` 的图片桥接 payload 是否已有明确 `tool_choice=image_generation` 或 `tools` 语义；若有，则把自动注入收口为“仅显式图片意图请求生效”，并补普通文本请求不注入测试。

### H3: 如果图片桥接 payload 已经显式声明 `tool_choice=image_generation`，那么代理层根本不需要对普通文本请求做兜底注入
- Supports: review 提到普通文本请求的工具集合会被静默扩大；如果图片入口已显式带工具意图，则缩窄到显式图片请求即可同时满足图片功能与普通文本语义稳定。
- Conflicts: 仍需确认 direct `/v1/responses` 图片请求是否也会显式带该 tool_choice。
- Test: 检查 `buildImagesResponsesRequest` 与相关 tests，确认图片请求的最小稳定标记。

## Experiments
- E1: 将 `internal/api/middleware/request_logging.go::isResponsesWebsocketUpgrade` 的路径识别从仅 `/v1/responses` 扩到同时接受 `/backend-api/codex/responses`，并在 `internal/api/middleware/request_logging_test.go` 补 alias websocket GET 用例。结果：`go test ./internal/api/middleware -run 'TestShouldSkipMethodForRequestLogging' -count=1` 通过，说明 alias GET 已重新进入 request logging 主链。
- E2: 将 `internal/logging/gin_logger.go::aiAPIPrefixes` 补入 `/backend-api/codex`，并在 `internal/logging/gin_logger_images_test.go` 增加 alias 路径断言。结果：`go test ./internal/logging -run 'Test(IsAIAPIPathIncludesImages|IsAIAPIPathIncludesCodexAliasResponses|GinLogrusLogger_MainLogIncludesUserAgent|SummarizeUserAgentForMainLog_NormalizesAndTruncates)' -count=1` 通过，说明 alias POST/compact 已重新拿到 AI API request_id 口径。
- E3: 将 `internal/runtime/executor/codex_image_tool.go` 的注入条件从“非 spark 一律注入”收口为“请求显式声明 image_generation 意图（`tool_choice.type=image_generation` 或已有 image_generation tool）时才补齐工具”，并补普通文本请求不注入测试。结果：`go test ./internal/runtime/executor -run 'Test(CodexPrepareRequestPlan_(InjectsImageGenerationToolForExplicitImageIntent|DoesNotInjectImageGenerationToolForPlainTextRequest|DoesNotDuplicateImageGenerationTool|SkipsImageGenerationToolForSpark|StripsStreamOptions)|ParseRetryDelaySupports(SecondsMessage|HumanDurationMessage))' -count=1` 通过，说明普通文本请求的工具集合不再被静默扩大。

## Root Cause
- Root cause 1: alias 路由只补了 `internal/api/server.go::setupRoutes`，但没有同步扩展 `internal/api/middleware/request_logging.go::isResponsesWebsocketUpgrade` 与 `internal/logging/gin_logger.go::aiAPIPrefixes` 的路径白名单，导致新入口功能可用但观测链路断开。
- Root cause 2: `internal/runtime/executor/codex_image_tool.go::ensureCodexImageGenerationTool` 过去只按模型后缀过滤，没有读取请求本身的图片意图，导致所有非 spark 的普通 Codex 请求都会被静默扩展工具集合。

## Fix
- Fix 1: alias Responses 的 GET/POST/compact 现在都重新纳入现有 request logging / request_id 主链。
- Fix 2: image_generation 注入现在只对显式图片请求生效；普通文本请求维持原始工具集合不变。

## 2026-04-25 v6.9.37 review fix

### Observations
- 当前 `disallow_free_auth` 只在 `sdk/api/handlers/openai/openai_images_handlers.go` 的 Images 路由写入；普通 Codex 显式图片意图请求不会带这个 metadata。
- 当前 Codex 图片工具注入真相源仍是 `internal/runtime/executor/codex_request_plan.go -> ensureCodexImageGenerationTool(...)`，selection 发生在此之前。
- `sdk/cliproxy/auth/conductor.go::isFreeCodexAuth` 只读取 `auth.Attributes["plan_type"]`，未复用仓内统一的 `AuthChatGPTPlanType(auth)` 回退逻辑。

### Hypotheses
- H1(ROOT): 需要在 handlers 层对“显式图片意图请求”提前写入 `DisallowFreeAuthMetadataKey`，否则 selection 阶段无法跳过 free-tier Codex。
- H2: `isFreeCodexAuth` 应该改为复用 `AuthChatGPTPlanType(auth)`，否则 metadata-only 的 free auth 会漏过过滤。
- H3: 只改 auth selection 不够，还要补 handlers/route 测试，分别覆盖普通图片意图 metadata 写入与 metadata-only free auth 的跳过。

### Experiments
- E1: 在 handlers 执行链增加“显式图片意图 -> disallow_free_auth” metadata 注入，并补最小测试验证。
- E2: 将 `isFreeCodexAuth` 改为复用 `AuthChatGPTPlanType(auth)`，再补 metadata-only free auth 的回归测试。

- E1 结果：在 `sdk/api/handlers/handlers.go::{ExecuteWithAuthManager,ExecuteCountWithAuthManager,executeStreamWithResolvedRoute}` 统一调用 `applyRequestIntentMetadata(...)`，对显式 `image_generation` 意图请求提前写入 `DisallowFreeAuthMetadataKey`。同时把请求意图判断抽到 `sdk/cliproxy/executor::RequestHasExplicitImageGenerationIntent(...)`，并让 `internal/runtime/executor/codex_image_tool.go::ensureCodexImageGenerationTool` 复用同一口径。验证：
  - `timeout 60s go test ./sdk/cliproxy/executor -run 'TestRequestHasExplicitImageGenerationIntent' -count=1` 通过；
  - `timeout 60s go test ./sdk/api/handlers -run 'Test(ExecuteWithAuthManager_DisallowFreeCodexForExplicitImageIntent|ExecuteCountWithAuthManager_DisallowFreeCodexForExplicitImageIntent|ExecuteStreamWithAuthManager_DisallowFreeCodexForExplicitImageIntent|RequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey|RequestExecutionMetadataIncludesDisallowFreeAuth)' -count=1` 通过；
  - `timeout 60s go test ./internal/runtime/executor -run 'TestCodexPrepareRequestPlan_(InjectsImageGenerationToolForExplicitImageIntent|DoesNotInjectImageGenerationToolForPlainTextRequest|DoesNotDuplicateImageGenerationTool|SkipsImageGenerationToolForSpark)' -count=1` 通过；
  - `timeout 60s go test ./sdk/api/handlers/openai -run 'TestImagesGenerations_(RoutesByImageModelAndExecutesWithMainModel|DisallowFreeCodexAuthDuringSelection)' -count=1` 通过。
- E2 结果：`sdk/cliproxy/auth/conductor.go::isFreeCodexAuth` 已改为复用 `sdk/cliproxy/auth/registration_order.go::AuthChatGPTPlanType(auth)`，metadata-only 的 `plan_type=free` 现在也会被 `disallow_free_auth` 正确跳过。验证：`timeout 60s go test ./sdk/cliproxy/auth -run 'TestManager_PickNextMixed_DisallowFreeAuthSkips(CodexFreePlan|MetadataOnlyCodexFreePlan)' -count=1` 通过。

### Root Cause
- Root cause 1: `disallow_free_auth` 之前只在 OpenAI Images 专用路由写入，普通 `Responses/Chat` 这类显式图片意图请求在进入 auth selection 时没有任何 free-tier 过滤信号。
- Root cause 2: `sdk/cliproxy/auth/conductor.go::isFreeCodexAuth` 自己重复实现了 `plan_type` 判断，却没有复用仓内统一的 `AuthChatGPTPlanType(auth)`，导致 metadata-only 的 free auth 与其他 Codex free 语义不一致。

### Fix
- Fix 1: 新增 `sdk/cliproxy/executor/request_intent.go`，把“显式图片意图”抽成共享 helper；handlers 选 auth 与 Codex request-plan 图片工具注入现在复用同一真相源。
- Fix 2: handlers 的三条执行链现在都会在 selection 前对显式图片请求补 `DisallowFreeAuthMetadataKey`，因此普通 Codex 图片意图请求与 `/v1/images/*` 一样，都会跳过 free-tier Codex auth。
- Fix 3: `sdk/cliproxy/auth/conductor.go::isFreeCodexAuth` 现在复用 `AuthChatGPTPlanType(auth)`，统一 Attributes 与 Metadata 的 `plan_type` 读取口径。

### Verification
- `timeout 60s go test ./sdk/cliproxy/executor -run 'TestRequestHasExplicitImageGenerationIntent' -count=1` 通过。
- `timeout 60s go test ./sdk/api/handlers -run 'Test(ExecuteWithAuthManager_DisallowFreeCodexForExplicitImageIntent|ExecuteCountWithAuthManager_DisallowFreeCodexForExplicitImageIntent|ExecuteStreamWithAuthManager_DisallowFreeCodexForExplicitImageIntent|RequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey|RequestExecutionMetadataIncludesDisallowFreeAuth)' -count=1` 通过。
- `timeout 60s go test ./internal/runtime/executor -run 'TestCodexPrepareRequestPlan_(InjectsImageGenerationToolForExplicitImageIntent|DoesNotInjectImageGenerationToolForPlainTextRequest|DoesNotDuplicateImageGenerationTool|SkipsImageGenerationToolForSpark)' -count=1` 通过。
- `timeout 60s go test ./sdk/api/handlers/openai -run 'TestImagesGenerations_(RoutesByImageModelAndExecutesWithMainModel|DisallowFreeCodexAuthDuringSelection)' -count=1` 通过。
- `timeout 60s go test ./sdk/cliproxy/auth -run 'TestManager_PickNextMixed_DisallowFreeAuthSkips(CodexFreePlan|MetadataOnlyCodexFreePlan)' -count=1` 通过。
- `timeout 60s go test ./... -run '^$'` 通过。
