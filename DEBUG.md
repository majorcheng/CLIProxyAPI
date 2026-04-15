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
