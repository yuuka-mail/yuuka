# 多账户整合

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 9. 多账户整合：统一视图，分级归属

典型的账户组合是三类：**多个 catch-all 域**（主力）、**工作邮箱**、**Gmail 之类的外部个人邮箱**。
目标是一个收件箱看全部、一处搜索、一套通知、一套 agent 面。

关键态度：**统一 ≠ 全量搬运**。统一的是视图、检索、通知和 agent 入口；
数据归属按账户分级（§9.2）。否则会把受合规约束的邮件和 Gmail 的全部历史
复制到本机，承担本来不必承担的合规和体量问题。

### 9.2 每个账户一套策略

```
accounts(kind, sync_policy, writeback_policy)
```

| 账户 | kind | sync_policy | writeback | 理由 |
|---|---|---|---|---|
| example.com 等 catch-all 域 | `cf_catchall` | `full`（存原件） | — | 自有域，数据全量自持 |
| Gmail | `imap`（后可换 `gmail_api`） | `full` 或 `metadata` | 已读/归档可选回写 | 量大，是否全量留存按需决定 |
| 工作邮箱 | `imap` | **`metadata` 或 `notify`** | 默认关 | 正文按需拉取不落盘，合规上保守 |

- `full`：原件进 R2 + 本地，完全等同自有邮件
- `metadata`：只存头部/摘要/索引，点开时实时拉正文，不落盘
- `notify`：只做通知和提醒，不建索引

**回写默认关闭**，单向拉取。开启后只允许 `read / archive / label` 三种，
且本地 flag 单独一层——本地 UI 里的标注永远不会被远端覆盖掉。
双向同步是这类系统最大的 bug 来源，这里用"默认单向 + 显式开白名单"换稳定。
从远端**删除**不属于回写，它是另一条独立策略（§9.6 的归档卸载），因为它不可逆。

### 9.3 发信：身份绑定传输（这条不照做一定出事）

identity → 出口必须是硬绑定：

| 身份 | 传输 | 原因 |
|---|---|---|
| `*@example.com` 及其他自有域 | Cloudflare Email Sending (SMTP 465) | 域已 onboard，任意前缀可发 |
| Gmail 地址 | Gmail 自己的 SMTP / API | 用别的出口发 = SPF/DKIM/DMARC 直接挂，进垃圾箱 |
| 工作邮箱 | 工作服务器 | 同上，且多半有审计要求 |

- 回复时**默认身份 = 这封信是从哪个投递进来的**（`deliveries.zone/prefix`
  或 account），不是全局默认。这是"多账户串身份"最常见的事故，在数据模型里就堵掉。
- 谁能用哪个 identity 发信由 `identity_grants` 决定（§13.5），
  服务主体无法冒用运维者的地址。
- outbox 每条带 `identity_id`，路由由 identity 决定，不给调用方选 transport 的机会；
  identity 可配 primary + fallback 传输链，前提是 DNS 已为两者授权（§10.4.4）。
- 出口按名字定义在进程配置里（主机、端口、caps、凭据文件路径；变量形状见 §10.2），`identities.transport` 只存名字，
  不建 `transports` 表（理由见 §4 的迁移说明）。传输链也在配置里按名字定义，identity 照样只引用一个名字。
- identity 是精确地址或 `*@zone`。精确地址压过同 zone 的通配：给服务的授权应当落在精确地址上，
  `*@zone` 上的授权碰不到别人单独拥有的地址（表见 [0005](../internal/store/migrations/0005_sending.sql)）。
- 每个自有域都要单独在 Cloudflare onboard Email Sending + 配 DNS；
  注意域名 30/zone（Routing+Sending 合计）、destination 200/account 的限制。

### 9.4 Agent 面的变化

§8 的东西基本原样成立，但多账户加三件事：

- 工具参数普遍多一个 `account?` / `zone?` 维度；默认**跨账户搜索**
  （统一的价值就在这），要限定时显式传
- `sync_policy=metadata/notify` 的账户，agent 默认拿不到正文——
  正文拉取算一次"外部请求"，要人授权或按白名单
- 某个账户是否可被某个主体读取，由 `account_access` 控制（§13.8）；
  ACL 粒度到账户为止，不下沉到单封信
- 受合规约束的账户内容进 agent 上下文这件事本身要可审计：`actions` 日志里
  记下哪个 agent 读了哪个账户的什么（§8.8），这在合规上比技术上更要紧

### 9.5 落地顺序

放在 **M3.5**（§6）：等解析、存储、规则引擎稳定后再接外部账户。
原因是 IMAP 拉取的脏活（UIDVALIDITY 变化、Gmail All Mail 重复、
OAuth 续期、限速）会掩盖核心链路的问题，不适合早做。
数据模型的**形状**从一开始就是多账户的：内容 / 投递两层拆分、`deliveries` 带账户维度、
幂等键是 `(账户, source_key)`，catch-all 只是其中一组账户。
但 `accounts` / `identities` 两张表在 **M2** 建（[0005](../internal/store/migrations/0005_sending.sql)）：
M0、M1 只有一个 Cloudflare 账户，用进程配置的 `account_key` 代替；
M2 发信时要按 identity 选出口、按 `identity_grants` 放行，这两张表才有了真正的读者。
0005 把每个 `account_key` 变成一个 `cf_catchall` 账户，`deliveries` 改用外键，幂等键改为 `(account_id, source_key)`。
`YUUKA_ACCOUNT` 从此是这个账户的名字，ingest 第一次用到时建行（§4）。
`accounts` 只建了 M2 读的列，kind 只有 `cf_catchall`，M2 的所有 identity 都属于这个账户；
只发不收的账户和拉取账户，连同 `sync_policy` 等列，随 M3.5 一起加。

### 9.6 归档卸载：yuuka 作为外部邮箱的容量外置

有些外部邮箱有容量上限。需求是：yuuka 把它们的旧邮件**完整接管**，然后把远端的那份删掉，
释放容量——yuuka 从"统一视图"升级成这些账户的**唯一长期存储**。

这是整份设计里**唯一对远端做不可逆动作**的地方，所以它不是 `writeback_policy` 的一个选项，
而是单独一条策略，默认关闭，按账户显式开启：

```
accounts.offload_policy = off | after(age=90d, require_flags=seen, exclude_labels=[...])
```

**前提**：`sync_policy` 必须是 `full`。`metadata` / `notify` 账户不可能开卸载——
你没有正文，删了就是真丢。工作邮箱若受合规约束，容量问题应由那边解决，不在这里卸载。

**删除闸门**（每一步都要留痕，缺一步不删）：

1. 该投递的 raw 已落 **R2 和本地两处**，`raw_objects.sha256` 已记录
2. **二次独立校验，和删除在同一个会话、同一个已验证的定位上完成**：`SELECT` 后核对 UIDVALIDITY
   （有稳定 id 的来源再核对 `X-GM-MSGID` / `EMAILID`）→ 按 UID 重拉 `BODY.PEEK[]`，
   sha256 与库里一致 → `UID STORE +FLAGS (\Deleted)` → `UID EXPUNGE`（§10.1）。
   防的是首次拉取时的截断、编码损坏，以及定位已经指向别的信。校验和删除分在两个会话做，
   中间 UIDVALIDITY 一变，校验过的和删掉的就可能不是同一封；同一个会话里同一个 UID 的内容不可变，
   校验过的就是删掉的
3. 该 raw 已被至少一次 **Postgres 备份快照覆盖**（§7 的备份是正经备份，这里是它的直接消费者）：
   卸载的信在库里的 `deliveries` 行是它存在过的唯一凭据
4. 命中 `offload_policy` 的年龄/标志/排除条件；`message_state.pinned`（钉住）的不删
5. 删除动作本身走 driver 的 `Expunger`（§10.1），记入 `actions`，actor 是 `system`，
   `deliveries.remote_state = offloaded`，`source_key` 和最后一次验证过的定位都保留——
   信从 Trash 恢复、或在别的文件夹再出现时，按 `source_key` 认得出是同一封，不会误判为新信

**没有任何 API 能按单封触发远端删除。** 卸载是策略驱动的系统批处理，不是 MCP tool，
也不是 WebUI 按钮；§13.4 的 scope 里没有它，agent 和服务主体连提案都提不了。
开关本身受 `account:manage` 管，改策略是一次需要 `session` 凭据的动作。

**Gmail 的语义要单独对待**：IMAP 里对 Gmail 做 `\Deleted + EXPUNGE` 只是去掉当前 label，
信还在 All Mail 里占容量；真正释放要么移到 `[Gmail]/Trash` 再 expunge（30 天后才释放），
要么用 Gmail API 的 `messages.delete`。driver 的 `Caps` 里要有 `ExpungeSemantics`，
UI 据此告诉用户"容量什么时候真正回来"，而不是假装都一样（§10.4.3）。

**卸载之后**：这封信在 yuuka 里的展示和自有域邮件完全相同（线程、搜索、agent 可引用），
`deliveries.remote_state` 让 UI 能显示"原件已从 Gmail 卸载"。
被卸载的邮件再也不会出现在那边的客户端里，这是需求本身，不是副作用；
反向（把 yuuka 里的信推回远端）**不做**——那是另一套同步系统。

落地放 **M3.5 之后**，等 IMAP 拉取跑稳一个周期再开；
`offload_policy`、`deliveries.remote_state` / `offloaded_at` 随 M3.5 的 IMAP 拉取一起建：只有外部账户才会被卸载。

---
