# Codex RT 逻辑备查

> 本文档只整理当前仓库里 **Codex refresh token（RT）相关的触发链与默认判据**，便于后续排障、核对日志与回归时快速对照源码。

## 1. 主要入口

### 1.1 新文件首次入池的一次性初始 refresh

- 入口：`sdk/cliproxy/service.go::applyCoreAuthAddOrUpdate`
- 调度：`sdk/cliproxy/auth/conductor.go::TriggerCodexInitialRefreshOnLoadIfNeeded`
- 开关：`config.example.yaml` 中的 `codex-initial-refresh-on-load`

行为：

1. 只有 **新加入的 Codex auth 文件** 才会在入池时被打上待处理标记。
2. 只有当 `codex-initial-refresh-on-load: true` 时，才会触发这条一次性后台 refresh。
3. 这条链成功后会清掉 pending 标记；命中终态失败也会清掉，避免同一份文件被反复当成“首次读取”。

这条链的目的不是“每次重载都先试一遍”，而是给**新文件首次入池**做一次凭证校准，把最新的 `access_token`、`refresh_token`、`expired`、`last_refresh` 回写到磁盘。

---

### 1.2 周期 auto-refresh

- 启动：`sdk/cliproxy/service.go::Run`
- 后台循环：`sdk/cliproxy/auth/conductor.go::StartAutoRefresh`
- 检查调度：`sdk/cliproxy/auth/auto_refresh_loop.go::nextRefreshCheckAt`
- 刷新判定：`sdk/cliproxy/auth/conductor.go::shouldRefresh` / `shouldRefreshCodexFromTokenJSON`

当前服务启动后，会开启 core auth manager 的后台 auto-refresh。  
对于 Codex，默认不是“看到 refresh_token 就主动打上游”，而是走**保守门控**：

1. **必须先有 `refresh_token`**
2. 若仍带有“初始 refresh 待处理”标记，则立即进入 refresh
3. 若显式设置了 `refresh_interval_seconds` / `refresh_interval`，优先按显式间隔
4. 否则走默认 Codex 判据：
   - `access_token` 的 JWT `exp <= now + 12h`
   - 或 metadata 中的 `expired <= now + 12h`
   - 或 `last_refresh >= 8 天前`
5. 如果既没有可解析的 `access_token exp`，也没有 `expired`，也没有 `last_refresh`，则**保持静默**，不做探测式 refresh

也就是说，当前默认逻辑已经从“离过期 3 小时就提前刷”调整为“离过期 **12 小时** 就提前刷”。

---

### 1.3 请求侧 401 的 refresh-retry 恢复

- 入口：`sdk/cliproxy/auth/conductor.go::executeWithCodex401Recovery`
- 同步 refresh：`sdk/cliproxy/auth/conductor.go::RefreshAuthNow`

当 Codex 请求本身返回 401，且当前 auth 仍带有 `refresh_token` 时，会走一条**现场恢复链**：

1. 首次请求命中 401
2. 同步触发一次 `RefreshAuthNow`
3. refresh 成功后，只重试 **1 次**
4. 如果 refresh 阶段直接得到终态 401，会归一成机器错误码，例如：
   - `codex_refresh_token_expired`
   - `codex_refresh_token_reused`
   - `codex_refresh_token_revoked`
   - `codex_refresh_unauthorized`
5. 如果 refresh 成功但重试后仍然 401，则返回 `codex_unauthorized_after_recovery`

这条链和后台 auto-refresh 不是一回事：  
**它是请求路径里的现场补救，而不是周期任务。**

---

### 1.4 Management 手动刷新

- 路由：`POST /v0/management/auth-files/codex/refresh`
- 处理器：`internal/api/handlers/management/auth_files_codex_refresh.go::RefreshCodexAuthFile`
- 核心执行：`sdk/cliproxy/auth/conductor.go::RefreshAuthNow`

管理面手动刷新会同步等待 refresh 完成，并把最新 auth 状态直接返回给前端。  
它主要服务于“单卡片手动刷新”这种运维场景，不依赖后台周期任务。

## 2. 默认触发规则

### 2.1 默认 proactive refresh 窗口

代码位置：`sdk/cliproxy/auth/conductor.go::codexProactiveRefreshWindow`

当前值：**12 小时**

默认顺序如下：

1. 优先从 `Metadata["access_token"]` 解析 JWT `exp`
2. 如果 JWT 解析不到，再回退到 `Metadata["expired"]`
3. 如果前两者都没有，再回退到 `last_refresh + 8 天`
4. 如果三个时间信号都没有，则默认不主动 refresh

### 2.2 显式 refresh_interval 的优先级

代码位置：

- `sdk/cliproxy/auth/conductor.go::authPreferredInterval`
- `sdk/cliproxy/auth/auto_refresh_loop.go::nextCodexRefreshCheckAt`

如果 auth metadata / attributes 中显式提供了 `refresh_interval_seconds`、`refresh_interval` 等字段，那么会覆盖默认的 12 小时窗口，直接按显式间隔判定。

这时：

- `exp <= now + interval` 时会刷
- 或 `last_refresh >= interval` 时会刷

## 3. 并发、退避与日志

### 3.1 防并发保护

代码位置：

- `sdk/cliproxy/auth/conductor.go::markRefreshPending`
- `sdk/cliproxy/auth/conductor.go::runRefreshAuth`
- `sdk/cliproxy/auth/conductor.go::tryAcquireRefreshSlot`

当前有两层保护：

1. `NextRefreshAfter` 会先打一个短暂 pending backoff，避免同一 auth 被同时反复调度
2. `refreshInFlight` 会阻止同一个 auth 并发使用同一份 RT

### 3.2 失败退避

代码位置：`sdk/cliproxy/auth/conductor.go::executeRefreshAuth`

- refresh 执行失败后，会把 `NextRefreshAfter` 推到 `now + 5 分钟`
- 若是“新文件初始 refresh”且只是瞬态失败，则后续按退避继续重试
- 若是“新文件初始 refresh”且已命中终态失败，则清掉 pending 标记，停止继续初始 refresh

### 3.3 非 debug 日志

代码位置：`sdk/cliproxy/auth/conductor.go::executeRefreshAuth`

当前 manager 刷新链会输出正式日志：

- 成功：`auth manager: rt 交换完成`
- 失败：`auth manager: rt 交换失败`

并带 `rt_rotated=true/false`，只表示 RT 是否发生轮换，不泄露 token 原值。

## 4. 与 Antigravity 的区别

虽然仓库里也有另一条 RT 交换链，但它**不是 Codex 这套 12 小时判据的一部分**。

- 代码位置：`internal/runtime/executor/antigravity_executor.go::ensureAccessToken`
- 规则：当 access token 缺失，或 token 过期时间已经接近 `refreshSkew`（当前约 3000 秒）时，执行期内联 refresh

所以如果你看到日志是：

- `auth manager: rt 交换完成`

那是 core auth manager / Codex 这条链。  
如果看到的是：

- `antigravity rt 交换完成`

那是 Antigravity executor 的运行时内联 refresh，不受上面的 12 小时窗口控制。

## 5. 排障时建议先看什么

1. **先分清触发入口**
   - 新文件首次入池
   - 周期 auto-refresh
   - 请求侧 401 恢复
   - management 手动刷新
2. **再看时间信号是否完整**
   - `access_token` 能否解析出 JWT `exp`
   - `expired` 是否存在
   - `last_refresh` 是否存在
3. **再看日志**
   - `auth manager: rt 交换完成`
   - `auth manager: rt 交换失败`
   - `codex 请求命中 401，开始 refresh-retry 恢复`
   - `codex 401 恢复耗尽，重试后仍返回 401`
4. **最后看错误码**
   - `codex_refresh_token_expired`
   - `codex_refresh_token_reused`
   - `codex_refresh_token_revoked`
   - `codex_refresh_unauthorized`
   - `codex_unauthorized_after_recovery`
