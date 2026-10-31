# 总体架构与设计原则

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 2. 总体架构

```
          ┌──────────────── Cloudflare ────────────────┐
  MX ───► │ Email Routing (catch-all *@example.com)       │
          │          │                                  │
          │          ▼                                  │
          │   Email Worker  (薄，~150 行 TS，不解析)    │
          │     1. 写 pending/<id> 标记对象              │
          │     2. raw MIME ──PUT──► R2 raw/YYYY/MM/..  │
          │     3. HMAC 签名 webhook ──┐                │
          │     4.（兜底）直推 TG 一行 │                │
          │                            │                │
          │   Email Sending (SMTP 465 / API)  ◄───┐     │
          │   Access / Tunnel ─────────┼──────────┼──┐  │
          └────────────────────────────┼──────────┼──┘  │
                                       ▼          │     │
          ┌──────── 自己的机器 (Go, 常驻) ─────────┴─────┴──┐
          │  ingest ──► parse ──► classify ──► notify        │
          │     │         │          │           └► TG / 其他 IM
          │     │         │          └► 规则引擎 / 验证码提取 │
          │     │         └► go-message                      │
          │     └► 幂等 + reconcile(定时扫 pending/)          │
          │  store: Postgres(元数据/索引) + 本地 raw 副本     │
          │  api:   REST(内部) + JMAP 子集(对外) + MCP        │
          │  out:   outbox 状态机 ──► CF SMTP                 │
          │  web:   WebUI (嵌入 Go 二进制)                    │
          └───────────────────────────────────────────────────┘
```

**Worker 为什么必须薄**：它是唯一不能用 Go 写的部分，所以把它压到"落盘 + 通知"
两件事，不放任何业务语义。MIME 解析只在 Go 侧有一份实现，WebUI、JMAP、
本地客户端看到的语义必然一致。

**为什么不用 Cloudflare Queues 当主通道**：R2 + `pending/` 标记已经是一个
可靠队列，且 R2 本身就是所需的归档层，少一个组件。webhook 只负责把延迟从
"下次 reconcile"降到"秒级"，丢了不影响正确性。以后量大再换 Queues 不动上层。

### 2.1 收信提交协议

信封（`MAIL FROM` / `RCPT TO`）不在 `.eml` 里，而 catch-all 判前缀靠的正是 `RCPT TO`。
所以 Worker 落盘的不只是原件，还有信封。顺序是唯一的，不留备选：

1. **写 `pending/<id>`**：JSON，含 `id`、`envelope_from`、`envelope_to`、`received_at`、
   `raw_key`。写失败就抛错
2. **PUT `raw/YYYY/MM/<id>.eml`**：同一份信封再写进 R2 对象的 custom metadata，
   让原件自描述——即使标记丢了，只凭 raw 也能恢复投递事实。写失败就抛错
3. 发 HMAC webhook（尽力而为，失败不抛错）
4. 正常返回 = 向 Cloudflare 确认收下

**Worker 侧的提交点是第 2 步成功。** 之前任何一步失败都抛错，交给 Cloudflare 让发件方重投
（这条行为尚待 M0 实验确认，见 §3 原则 5）。

**Go 侧的提交点**是一个事务：写 `raw_objects` + `deliveries`（信封从标记或 metadata 读）。
事务提交后才删除 `pending/<id>`。reconcile 按标记的状态处理：

| 标记 | raw | 处理 |
|---|---|---|
| 有 | 有 | 正常 ingest（幂等），提交后删标记 |
| 有 | 无，标记 < 15 分钟 | Worker 可能还在写，跳过 |
| 有 | 无，标记 ≥ 15 分钟 | Worker 在第 2 步失败并已抛错，这封由发件方重投、会换新 id。记一条 `orphan_marker` 事件后删标记；短时间内频繁出现就告警 |
| 无 | 有，库里没有 | 不应出现（标记只在入库后删）。每周一次全量扫 `raw/` 与 `raw_objects` 对账兜底，信封从 metadata 恢复 |

注意 Email Worker 每次调用只有**一个** `RCPT TO`：同一封信发给同域两个前缀，
Worker 会被调用两次、产生两份 raw，由 §4.1 的折叠合并成一条 message、两条 delivery。

**机器在 NAT 后面**：用 `cloudflared` tunnel 暴露 ingest 和 WebUI；
WebUI 前面挂 Cloudflare Access，ingest 端点靠 HMAC + 时间戳防重放。

---

---

## 3. 八条设计原则

1. **原始 MIME 是邮件内容的唯一事实来源，不可变**
   `raw/YYYY/MM/<ulid>.eml` 写一次永不改。从内容派生的一切（解析结果、线程、附件索引、
   提取值）都能用 `reparse` 一条命令整库重放，解析器出 bug 不等于丢信息。
   但**人和系统做出的决定不是派生物**：标注、改判、前缀登记、规则、授权、
   投递事实与远端卸载状态。它们和派生数据分开存放、`reparse` 不碰、按正经数据库备份（§4.2）。

2. **`(域, 前缀)` 是维度，不是邮箱**
   不存在"创建邮箱"这个动作。每个 catch-all 域的 `*@<zone>` 全部接住，
   未见过的组合自动进 `prefixes(zone, prefix)` 表。全局单库 →
   跨域跨前缀搜索、统计、去重天然可做。

3. **前缀注册表 + 泄漏检测**（现成项目全都没有，但这是 catch-all 的真正价值）
   每个前缀登记"我把它给了谁"。当 `github@` 收到非 GitHub 发件域的信，
   或某前缀的发件域集合突然变化 → 标记"可能泄漏/被转卖"。
   这里可以复用 `ix64/spf`：对 `Authentication-Results` 之外再做一次
   SPF 判定，用于"自称是 X 的信到底是不是 X"的判断。
   前提是输入可信：发信方 IP、HELO 只从本账户可信接收方加的那一行 `Received` 里取，
   任何认证头都可能是发件人伪造的（§10.1.1）。输入不齐时判定结果是 `none`，不是 `pass`。
   前缀分两态：**claimed**（我认领过、给出去过）和 **discovered**（只是有人往这投过）。
   泄漏检测、TG 推送、聚合视图默认只看 claimed；discovered 主要是垃圾信字典攻击的产物，
   静默登记即可，认领是一个显式动作（§4.3、`prefixes.state`）。

4. **双区分流（machine / human）**
   `class` 字段 + 规则引擎，信号：`List-Unsubscribe`、`Auto-Submitted`、
   `Precedence: bulk`、`noreply` 类发件人、无 `In-Reply-To` 且命中验证码模式、
   前缀命名约定（`svc-*` / `gh-*`）。
   - 通知区：按前缀/服务聚合，自动提取验证码、magic link、过期时间，
     只读、可批量清理、推送到 TG；UI 重点是"一眼看到码"而不是读信。
   - 会话区：完整 MUA 语义——线程、引用、草稿、附件、多身份签名、可回复。
   分流错了可以一键改判并生成规则，规则是用户数据不是代码。

5. **不丢信**
   未知前缀照收；解析失败进 `quarantine` 并保留 raw；ingest 幂等
   （折叠键 `(message_id, body_hash)` + 同路径 raw sha256 去重，见 §4.1；Email Worker 可能重投）;
   Worker 侧只要 raw 写成功就算收下，自建服务离线数天也只是延迟入库。
   提交顺序、信封存放和 reconcile 规则见 §2.1，只有那一套，不留备选。
   「Worker 抛错 → Cloudflare 让发件方重投」是这条原则的基石，但**官方文档没有写**
   （文档只说明 `setReject()` 给发件方永久错误）。M0 的第一个实验就是在 Worker 里故意抛错，
   看发件方拿到的是 4xx 还是静默丢弃；若是后者，Worker 在 R2 写失败时必须
   `message.forward()` 到一个备用地址兜底。实验结论写回本条

6. **出口是状态机，不是 fire-and-forget**
   `outbox(held → queued → sending → sent | partial | unknown | failed(n) → dead)`，
   结果按收件人逐个记录（`outbox_recipients`），bounce/suppression 回写到收件人这一层。
   **只在确定对方没收下时重试**：连接、认证、`RCPT` 阶段失败或 4xx 可以退避重试；
   正文（`DATA`）已经发出但没等到最终回复，就是 `unknown`——不自动重试，
   先用出口的状态查询或退信事件对账，对不上再交给人决定。盲目重试的代价是对方收到两封。
   部分收件人被拒是 `partial`：只对 4xx 的那几个收件人重试，已收下的不再发。身份层支持任意 `From` 前缀（Cloudflare Email
   Sending 是域级 onboard，所以 `任意@example.com` 都能发），签名/显示名按前缀配置。

7. **入口是多实例的 driver，不是一个管道**
   `ingest driver` 是接口，而且**从第一天起就有多个实例并存**：Cloudflare
   Email Worker（各 catch-all 域）、IMAP 拉取（Gmail / 工作邮箱）、
   将来可能的自建 MX。出口同理：SMTP 口是通用货币，`smtpTransport` 一个实现
   就覆盖 CF / Resend / SES / Gmail / 工作邮箱（§10）。上层不动。
   这条保证系统不被 Cloudflare 的 beta 功能和配额锁死。

8. **Agent 是一等公民，权限按身份分档**
   Agent 和人共用同一套领域 API、同一套鉴权、同一套审计，不开后门；
   能力分级由 API 层强制（见 §8），读随便读，标注可撤销，
   **外发按身份授权**：以自己的专属身份可以经自动审核或在限额内直接发出，
   以人的身份只能产生草稿（§8.9）。规则引擎没有发信身份，只做标注层动作（§8.5）。
   邮件正文永远按不可信输入对待。

---

---

## 5. Go 侧选型

| 用途 | 选择 | 备注 |
|---|---|---|
| MIME 解析 | `github.com/emersion/go-message` (+ `mail`) | 编码/多部分/inline 处理成熟；`enmime` 作为宽容兜底备选 |
| 发信 | `go-message` 组装 + `net/smtp` over TLS 465 | **MIME 自己组装，transport 只投递字节**；一个 smtp driver 覆盖 CF/Resend/SES/Gmail/工作邮箱，差别只是配置（§10.2）。不引第三方 SDK |
| **拉取**外部邮箱 | `go-imap/v2` client + IDLE，PLAIN 或 OAuth2(XOAUTH2) | M365 已关基础认证，必须 OAuth。Gmail 个人账户开两步验证后**仍可用 app password**，而 OAuth 同意屏在 Testing 模式下 refresh token 7 天过期、转正式又要应用审核——个人项目 Gmail 默认走 app password，OAuth 留给 M365。Gmail 以后可升级到 Gmail API（`history.list` 增量 + Pub/Sub push），增量比 IMAP 可靠 |
| 以后要真 IMAP（服务端） | `github.com/emersion/go-imap/v2` 的 server 侧 | **因为后端常驻，IMAP 永远是可加的一层，不依赖 CF 的私测功能** |
| JMAP | 自己实现子集 | 见下 |
| HTTP | `net/http` + `chi`/标准库 | 不需要重框架 |
| DB | `pgx` + 手写 SQL（或 sqlc） | 邮件查询形状怪，ORM 吃亏 |
| 前端 | 任选，产物 `embed` 进二进制 | 单文件部署 |
| SPF | `ix64/spf` | 同作者的既有库，直接复用 |

**关于 JMAP 要诚实说一句**：JMAP 客户端生态非常薄（Fastmail 自家客户端、
ltt.rs 等，Thunderbird / aerc / iOS Mail 都不支持）。所以"JMAP 就行"实际等于
"我自己写客户端或用 PWA"。建议的落法：
- 内部 API 自己定（REST，贴着 UI 需求走，不受 JMAP 束缚）
- 对外暴露 **JMAP 子集**：`Core/echo`、`Mailbox/get`、`Email/query`、`Email/get`、
  `Email/set`、`EmailSubmission/set`、`Email/queryChanges`（增量同步）
- 真的想用 Thunderbird/iOS Mail 的那天，加 `go-imap` 网关，一两周的活，
  后端模型已经就绪

---
