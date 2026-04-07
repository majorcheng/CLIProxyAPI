# Management API：Codex 手动 RT 刷新接口说明

> 本文档面向管理页面前端联调，说明如何调用后端新增的 Codex 手动刷新接口。

## 1. 接口用途

该接口用于对 **单个 Codex 凭证** 手动触发一次 refresh token 刷新，并且：

- **同步等待刷新完成**
- 成功后直接返回该凭证的最新状态
- 失败时尽量返回当前凭证状态，方便前端直接更新卡片

它适合管理页面上的“单卡片刷新”按钮场景，不需要额外任务轮询。

---

## 2. 路由与鉴权

### 路由

```http
POST /v0/management/auth-files/codex/refresh
```

### 鉴权

与其它 management API 完全一致，支持：

```http
Authorization: Bearer <MANAGEMENT_KEY>
```

或：

```http
X-Management-Key: <MANAGEMENT_KEY>
```

---

## 3. 请求体

请求体为 JSON，支持以下字段：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `auth_index` | string | 条件必填 | 推荐使用。来自 `GET /v0/management/auth-files` 返回的 `auth_index` |
| `authIndex` | string | 否 | `auth_index` 的 camelCase 别名 |
| `AuthIndex` | string | 否 | `auth_index` 的 PascalCase 别名 |
| `name` | string | 条件必填 | 凭证文件名或 auth ID，建议作为兜底字段一起传 |

### 目标解析规则

后端按以下顺序解析目标凭证：

1. 优先使用 `auth_index / authIndex / AuthIndex`
2. 如果没有传 `auth_index`，则使用 `name`
3. 如果 `auth_index` 和 `name` 同时传入，但指向的不是同一个凭证，则直接返回 `400`

### 推荐请求方式

前端建议：

- **优先传 `auth_index`**
- 同时附带 `name`，便于排障与日志定位

示例：

```json
{
  "auth_index": "6b8d5c1e2f3a4b5c",
  "name": "codex-user@example.com-plus.json"
}
```

---

## 4. 成功响应

成功时返回：

```json
{
  "status": "ok",
  "file": {
    "id": "codex-user@example.com-plus.json",
    "auth_index": "6b8d5c1e2f3a4b5c",
    "name": "codex-user@example.com-plus.json",
    "type": "codex",
    "provider": "codex",
    "label": "user@example.com",
    "status": "active",
    "status_message": "",
    "disabled": false,
    "unavailable": false,
    "source": "file",
    "has_refresh_token": true,
    "email": "user@example.com",
    "last_refresh": "2026-04-07T12:34:56Z"
  }
}
```

### `file` 字段说明

`file` 字段尽量复用 `GET /v0/management/auth-files` 的单项结构，前端可以直接拿它覆盖当前卡片数据。

常用字段建议重点关注：

- `auth_index`
- `name`
- `status`
- `status_message`
- `disabled`
- `unavailable`
- `has_refresh_token`
- `email`
- `last_refresh`

---

## 5. 失败响应

### 5.1 状态码约定

| 状态码 | 场景 |
| --- | --- |
| `400` | 参数错误、不是 Codex 凭证、缺少 `refresh_token` |
| `404` | 未找到目标凭证 |
| `409` | 该凭证当前已经在刷新中 |
| `503` | 后端没有可用的 Codex refresh executor |
| `502` | 上游 RT 刷新失败 |

### 5.2 失败时的响应结构

失败时始终会返回：

```json
{
  "error": "错误信息"
}
```

在以下场景中，后端还会尽量附带当前凭证快照：

- `409`：凭证正在刷新中
- `502`：上游 refresh 失败，但后端仍能拿到当前 auth 状态
- 其它能定位到当前 auth 的失败场景

例如：

```json
{
  "error": "Codex RT 刷新失败: refresh_token_reused",
  "file": {
    "id": "codex-user@example.com-plus.json",
    "auth_index": "6b8d5c1e2f3a4b5c",
    "name": "codex-user@example.com-plus.json",
    "type": "codex",
    "provider": "codex",
    "status": "active",
    "status_message": "",
    "disabled": false,
    "unavailable": false,
    "has_refresh_token": true
  }
}
```

---

## 6. 前端接入建议

### 6.1 按钮点击流程

推荐流程：

1. 用户点击单卡片“刷新”
2. 前端将该卡片按钮置为 loading / disabled
3. 调用本接口
4. 请求结束后取消 loading
5. 如果响应里有 `file`，直接用 `file` 覆盖当前卡片数据
6. 如果失败，再展示错误提示

### 6.2 为什么推荐直接用 `response.file` 回填

因为刷新成功后，后端可能会同步更新：

- `last_refresh`
- `status`
- `status_message`
- `disabled`
- `unavailable`
- 以及其它管理页已经展示的状态字段

直接用 `response.file` 覆盖当前卡片，能避免前端本地状态和后端状态不一致。

### 6.3 是否需要再额外刷新整个列表

通常 **不需要**。

这个接口已经返回了最新单项数据，适合作为“单卡片局部刷新”的直接数据源。  
只有在你的页面还依赖其它聚合数据时，才考虑再做一次全量列表拉取。

---

## 7. 前端调用示例

### fetch 示例

```ts
async function refreshCodexAuth(baseUrl: string, managementKey: string, file: {
  auth_index?: string
  name: string
}) {
  const response = await fetch(`${baseUrl}/v0/management/auth-files/codex/refresh`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${managementKey}`
    },
    body: JSON.stringify({
      auth_index: file.auth_index,
      name: file.name
    })
  })

  const data = await response.json()

  if (!response.ok) {
    const error = typeof data?.error === 'string' ? data.error : `HTTP ${response.status}`
    return {
      ok: false,
      status: response.status,
      error,
      file: data?.file ?? null
    }
  }

  return {
    ok: true,
    status: response.status,
    file: data.file
  }
}
```

### 推荐 UI 行为

- `200`：提示“刷新成功”，并更新当前卡片
- `409`：提示“该凭证正在刷新中”，并用返回的 `file` 更新卡片
- `502`：提示具体错误，例如 `refresh_token_reused`，如果有 `file` 也更新卡片
- `400/404/503`：提示错误即可

---

## 8. curl 调试示例

```bash
curl -X POST "http://127.0.0.1:8317/v0/management/auth-files/codex/refresh" \
  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "auth_index": "6b8d5c1e2f3a4b5c",
    "name": "codex-user@example.com-plus.json"
  }'
```

---

## 9. 重要边界说明

1. **本接口只支持 Codex**
   - 不是通用 OAuth 刷新接口
   - 非 Codex 凭证会返回 `400`

2. **本接口是同步接口**
   - 调用方会等待本次 refresh 完成
   - 不需要额外轮询任务状态

3. **同一凭证不允许并发 refresh**
   - 如果后台 auto-refresh 或另一条手动刷新已经在执行，会返回 `409`

4. **成功刷新后会落盘**
   - 后端会保证新的 RT/AT 持久化到 auth 文件
   - 前端不需要自己做任何“保存”动作

5. **失败时不代表前端必须全量重载**
   - 只要响应里带了 `file`，就优先使用该字段刷新当前卡片

---

## 10. 推荐前端最小接入策略

如果你只是想最快接上管理页按钮，推荐最小闭环如下：

1. 在卡片右上角增加“刷新”按钮
2. 点击后请求本接口
3. loading 期间禁用按钮
4. 成功后用 `response.file` 覆盖当前卡片
5. 失败时弹错误提示
6. 如果失败响应里也带了 `file`，同样覆盖当前卡片

这样就能做到：

- 不加新轮询
- 不加新任务系统
- 不强依赖全页 reload
- 保持和后端当前接口语义一致
