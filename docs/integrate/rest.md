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
- 会有新行插到头部的列表（消息、搜索、草稿、发件队列、提案）和变更（见下）用不透明游标分页；变化很慢的列表用 `limit` / `offset`
- 多值参数逗号分隔，如 `class=human,unknown`
- 字段名 snake_case，时间是 RFC 3339 UTC，列表永远是数组，响应里没有 `null`

受限的凭据能知道某封受限的信存在，但看不到它的任何头部和正文。

## 跟上变化

`GET /api/v1/changes` 列出某个游标之后变了的对象，按提交顺序，旧信上的变化也在内：
新信、晚到的分类、重算的验证码和链接、人或规则改的标签 / 分类 / 归档、规则回溯、提案的决定、草稿和发件队列。
每条只有 `kind`、`id`、`at`（投递另带 `message_id`），不带内容；要看就用对应的端点去读，
读到的可能已是更新的状态，或者 404（对象已不在）。

1. 先不带 `since` 调一次，拿到当前的 `cursor`
2. 再读你关心的现状（列表、搜索……）
3. 之后每次带 `since=<上次的 cursor>`；`more` 为真就立刻再要一次。一页可能很短甚至为空而 `more` 为真，这是正常的

先拿游标再读现状，中间的变化会重复看到，但不会漏。游标原样传回，不要解析它。

- 不给 `kinds` 就是你的凭据够得着的全部：`delivery`、`outbox` 要 `delivery:read`，`prefix` 要 `prefix:read`，
  `rule` 要 `rule:manage` 且只有人能看。显式要了够不着的 kind 回 `403 scope_required`
- `kinds` 滤掉的变化游标照样越过，改 `kinds` 要从第 1 步重来
- `410 cursor_expired`：游标接不上了（比如服务端从备份恢复过）。从第 1 步重来，并重读手上的东西
- 看不见的变化被跳过，游标照常前进：它只透露「有活动」，不透露是什么
