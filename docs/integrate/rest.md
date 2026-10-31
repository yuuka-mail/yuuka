# 接入 REST

REST 在 `<YUUKA_WEB_ORIGIN>/api/v1/` 下，契约是 OpenAPI 3.1，从 Go 类型生成，不手写：

- 运行中的实例：`GET /api/v1/openapi.json`，不需要认证
- 仓库里：[`internal/api/openapi.json`](../../internal/api/openapi.json)

端点、参数、响应的形状都以这份文档为准，这里只写跨端点的约定。为什么选 OpenAPI：[设计](../design/architecture.md#51-api-契约openapi-31从-go-类型生成)。

## 认证

程序用 bearer token：`Authorization: Bearer ykt_…`，由运维者签发，scope 按要调的端点给
（每个端点要哪个 scope 写在 OpenAPI 里；scope 列表见[接入 MCP](mcp.md#权限)）。
WebUI 用的是浏览器会话，程序不要模仿它。

## 约定

- **错误**是 RFC 9457 的 `application/problem+json`。按扩展成员 `code` 分支，它是稳定的机器可读码，取值在 schema 里列全；
  `detail` 是给人读的
- **超出授权不是错误**：提交草稿（`POST /api/v1/drafts/{id}/submit`）照常返回成功，结果里说明信是进了队列还是停在草稿、为什么
- **被更高等级覆盖也不是错误**：标注（`POST /api/v1/messages/{id}/annotations`）对每项返回 `applied`、`unchanged`、
  `overruled`（人改过的值，agent 改不动）或 `labels_full`
- 对象已不在可改的状态（草稿已提交、发件队列里的行不是 `unknown`、提案已决定）回 `409 conflict`；
  `from` 没有对应的发信地址回 `422 no_identity`；直接拒收回 `403 submission_rejected`
- 规则或提案校验不过回 `400 invalid_rule`，`path` 指向请求体里出错的位置，如 `match.all[1].value`；
  保存成功但有风险的规则（按 `From` 匹配却不要求 `from.verified`）在成功的响应里带 `warnings`
- 决定提案和管理规则只有人能做，别的主体即使 scope 齐全也回 `403 forbidden`；
  一个提议者待决的提案到了上限（50 个）回 `429 rate_limited`
- 创建回 `201` 和新对象，删除回 `204`。修改是 `PUT` 整体替换，没有 `PATCH`
- 会有新行插到头部的列表（消息、搜索、草稿、发件队列、提案）用不透明游标分页；变化很慢的列表用 `limit` / `offset`
- 多值参数逗号分隔，如 `class=human,unknown`
- 字段名 snake_case，时间是 RFC 3339 UTC，列表永远是数组，响应里没有 `null`

受限的凭据能知道某封受限的信存在，但看不到它的任何头部和正文。
