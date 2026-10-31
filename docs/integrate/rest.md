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
- **超出授权不是错误**：提交草稿（`POST /api/v1/drafts/{id}/submit`）照常返回成功，`state` 说明信是进了队列（`queued`）、
  审核通过后扣在撤回窗口里（`held`，到 `hold_until` 发出）还是停在草稿（`draft`，`hold_reason` 说为什么）。
  被审核拦下的草稿带 `review`：每项检查的结论和触发它的东西；验证码和登录链接只说在哪里找到，不带值。
  发现里指向的信（`extracted`、`refs` 的 `message_id`）在你看得见那封信时带它的 `subject`。
  `GET /api/v1/drafts?hold_reason=review,owner` 只列因这些原因停着的草稿
- **用某个身份发会怎样**：`GET /api/v1/identities` 每项的 `outcome` 直接说：`sends`（不经人发出）、
  `held_after_review`（先审核，过了扣在 `hold_seconds` 的撤回窗口里再发，拦下的停在草稿）、`draft_only`、`none`。
  `review` 档的 `sends` 是假、`hold_reason` 是 `review`，和被审核拦下的草稿一样，所以按 `outcome` 分支，不要从 `hold_seconds` 推
- **撤回**扣在窗口里的信：`POST /api/v1/outbox/{id}/withdraw`，要 `submission:send`。人能撤回看得见的任何一封，
  agent 和 service 只能撤回自己提交的（别人的回 `403 forbidden`）。信回到草稿（`hold_reason = withdrawn`），可以改了再提交。
  已经放行（`queued` 及之后）或已撤回的回 `409 conflict`。人撤回触发了熔断时，响应带 `suspended`：
  身份地址 `identity` 和主体 `principal`（`kind`、`name`），恢复就是 `yuuka grant set --identity <identity> --principal <kind>:<name> --resume`。
  没有让信提前发出或缩短窗口的端点
- **被更高等级覆盖也不是错误**：标注（`POST /api/v1/messages/{id}/annotations`）对每项返回 `applied`、`unchanged`、
  `overruled`（人改过的值，agent 改不动）或 `labels_full`
- 对象已不在可改的状态（草稿已提交、要 resolve 的行不是 `unknown`、要撤回的行不是 `held`、提案已决定）回 `409 conflict`；
  `from` 没有对应的发信地址回 `422 no_identity`；直接拒收回 `403 submission_rejected`
- 规则或提案校验不过回 `400 invalid_rule`，`path` 指向请求体里出错的位置，如 `match.all[1].value`；
  保存成功但有风险的规则（按 `From` 匹配却不要求 `from.verified`）在成功的响应里带 `warnings`
- 决定提案和管理规则只有人能做，别的主体即使 scope 齐全也回 `403 forbidden`；
  一个提议者待决的提案到了上限（50 个）回 `429 rate_limited`
- **笔记**（`PUT` / `DELETE /api/v1/messages/{id}/note`，`message:annotate`）：每个主体在每封信、每个线程上各一条，
  再写就覆盖，删掉就是撤回。`cites` 至少一处、最多 32 处，每处摘录最多 1000 字符（提案的 `cites` 同样）。
  agent 只能删自己的；人还能带 `author=<主体 id>` 删他看得见的信上别人的笔记，记成人的动作。读笔记在消息详情 `GET /api/v1/messages/{id}` 的 `notes` 里：
  人，以及读得到全部账户的 agent 和服务，看到信上的全部笔记；其他主体只看到自己的（别人的笔记可能引到它看不到的信）。
  笔记是作者的判读、不是 yuuka 的事实，展示时署作者名
- **决策模型的评分**（[§15](../design/triage.md)，只观测）：只在运维者配了评分器时出现，只认当前评分器。
  消息详情的 `triage_score` 是最新一次评分（各问题的概率或弃权、评分器、模型、时刻、`notice`），
  `input`（评分器看到的事实、字数、哈希、是否发给了第三方）只给有 `delivery:read`、看得到这封信全部投递的人。
  列表、线程和搜索命中带紧凑的 `triage`：每个问题概率最高的选项，弃权的问题不出现。
  `triage=<问题>:<选项>`（如 `phishing:yes`）按同一个定义过滤消息列表和搜索，不设阈值；问题或选项不在问题集里、
  或者没配评分器时回 `400 invalid_param`。`GET /api/v1/triage/report` 是 `yuuka triage report` 的同一份计数，只给人。
  这些都是模型的估计，展示时和系统事实分开
- `/api/v1/admin/` 下的端点（凭据、身份、授权）只认人在 WebUI 里的会话：bearer token 即使带着 `admin` 和其他全部 scope
  也回 `403 forbidden`，程序用不了它们。它们只能查看和收紧；会让某个主体能做更多的改动回 `403 widening_requires_cli`，
  `detail` 写出在主机上做这件事的 `yuuka` 命令（[为什么](../design/principals.md#1310-管理面收紧在界面放宽在主机)）
- 认领、取消认领前缀和改期望域只看 scope（`prefix:manage` 加 `prefix:read`），回答里带前缀现在的统计：
  认领与否、期望域都在读时现算，已收的信立刻按新决定显示
- 创建回 `201` 和新对象，删除回 `204`。修改是 `PUT` 整体替换，没有 `PATCH`；只改一部分、且有方向限制的改动是一个动作，
  如 `POST /api/v1/admin/grants/{identity}/{principal}/tighten`
- 会有新行插到头部的列表（消息、搜索、草稿、发件队列、提案）和变更（见下）用不透明游标分页；变化很慢的列表用 `limit` / `offset`
- 多值参数逗号分隔，如 `class=human,unknown`
- **侧栏计数**：`GET /api/v1/counts` 一次给出待决的提案、在写和停着的草稿，有 `delivery:read` 时再给发件队列里
  扣在撤回窗口的（`held`）、投递未知的（`unknown`）和要人看一眼的（`attention`：`unknown`、`failed`、`partial`）。
  每个数都是对应列表会给你看的行数
- 字段名 snake_case，时间是 RFC 3339 UTC，列表永远是数组，响应里没有 `null`

受限的凭据能知道某封受限的信存在，但看不到它的任何头部和正文。

## 跟上变化

`GET /api/v1/changes` 列出某个游标之后变了的对象，按提交顺序，旧信上的变化也在内：
新信、晚到的分类、重算的验证码和链接、人或规则改的标签 / 分类 / 归档、规则回溯、提案的决定、草稿和发件队列、笔记、
决策模型的新评分（`triage`，`id` 是信的 id）。
每条只有 `kind`、`id`、`at`（投递和笔记另带所在信的 `message_id`），不带内容；要看就用对应的端点去读，
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
