# 收发 Driver 契约与能力矩阵

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 10. Driver 契约与能力矩阵

**是的，收和发都是 driver，但"可替换"要说清楚边界。**

能替换的根本原因只有一个：**RFC 5322 的字节流是共同货币**。所有 driver 的职责
都压缩成"把 raw MIME 搬进来 / 搬出去"，解析、线程、分类、存储只有一份实现。
driver **一律不解析 MIME**——这是契约里最重要的一条。

漏出抽象的四件事（§10.4 细说）：Message-ID 归属、bounce 回流路径、
能力不对等、DNS 预授权。这四件不认真处理，driver 抽象就是假的。

### 10.1 收：Push 和 Pull 两种形状，一个产出

```go
// 所有 driver 的唯一产出：原件引用 + 幂等键 + 来源
type InboundRef struct {
    AccountID   AccountID
    RawKey      string      // 已落 RawStore 的 key；Completeness=None 时为空
    Completeness Completeness // Full | HeaderOnly | None，由账户的 sync_policy 决定（§9.2）
    Header      []byte      // 仅 None：只在内存里用于生成通知，不落盘
    Size        int64
    SHA256      [32]byte
    Mailbox     string      // IMAP 文件夹；UID 只在它和 UIDValidity 之内有意义。其他 driver 填 ""
    UIDValidity uint64      // IMAP 语义，其他 driver 填 0
    SourceUID   string      // IMAP UID / Gmail X-GM-MSGID 或 API id / Worker 生成的 ULID
    EnvelopeTo  []string    // RCPT TO，catch-all 判前缀靠它而不是 To 头
    EnvelopeFrom string
    ReceivedAt  time.Time
}

type Source interface {
    Account() AccountID
    Caps() SourceCaps
}

// 推：Cloudflare Email Worker / SES+SNS / Postmark / Mailgun / 自建 MX(LMTP)
type PushSource interface {
    Source
    // 验签 → 取 raw → 落 RawStore → 返回引用。失败必须让对方重投。
    Accept(ctx context.Context, r *http.Request) ([]InboundRef, error)
}

// 拉：IMAP / Gmail API / POP3 / Maildir / mbox 导入
type PullSource interface {
    Source
    Fetch(ctx context.Context, cur Cursor, limit int) ([]InboundRef, Cursor, error)
    Watch(ctx context.Context) (<-chan struct{}, error) // IMAP IDLE / Pub/Sub，可选
    // metadata 策略下按需取正文：流式返回，不进 RawStore，调用本身记入 actions（§9.4）
    FetchBody(ctx context.Context, loc RemoteLoc) (io.ReadCloser, error) // loc = (mailbox, uidvalidity, uid)
}

// 可选：把本地的已读/归档写回远端（§9.2 默认关）
type FlagWriter interface { SetFlags(ctx, []string, Flags) error }

// 可选：归档卸载（§9.6）。唯一不可逆的远端动作，只由系统批处理调用，没有 API 暴露它。
// Caps.ExpungeSemantics 说明"删了之后容量什么时候真正释放"（IMAP 立即 / Gmail 进 Trash 30 天 / 不支持）
type Expunger interface { Expunge(ctx, []string) error }
```

要点：
- **Push 的 ack 语义**：`Accept` 返回成功才算收下。Cloudflare Worker 侧只要
  R2 写成功就 ack，所以自建服务离线不丢信（§3.5）。
- **Pull 的游标**：`Cursor` 对 IMAP 是每个文件夹一份 `(UIDVALIDITY, lastUID)`，
  对 Gmail API 是 `historyId`，对 Maildir 是 mtime+inode。
- **幂等**：`unique(account_id, mailbox, uidvalidity, source_uid)` 在 `deliveries` 上（§4），
  driver 不需要自己记"处理过没有"。只用 `(account_id, source_uid)` 是错的：
  IMAP 的 UID 只在同一文件夹、同一 `UIDVALIDITY` 内唯一。
- **`UIDVALIDITY` 变了**：该文件夹全量重扫，按 raw sha256 找到同一账户同一文件夹里已有的
  delivery，**原地更新**它的 `uidvalidity / source_uid`，而不是插入新行。
- **Gmail 走 IMAP 时只同步 `[Gmail]/All Mail`**，`SourceUID` 用 `X-GM-MSGID`（跨文件夹稳定），
  label 用 `X-GM-LABELS` 取。否则同一封信在 INBOX 和 All Mail 里各算一次投递。
- **`metadata` / `notify` 策略不是"拉下来再丢"**：
  - `metadata`：driver 用 `BODY.PEEK[HEADER]` 只拉头部，`Completeness=HeaderOnly`，
    头部照常落 RawStore，解析器据此只建头部索引；点开时走 `FetchBody` 实时取（§4.4）
  - `notify`：`Completeness=None`，头部只在内存里生成一条通知，不落 RawStore、不建 message；
    `deliveries` 只留一行幂等用的桩
  否则 InboundRef「已落 RawStore」的契约和 §9.2「正文不落盘」直接矛盾。

#### 10.1.1 认证事实只信可信接收方

`Authentication-Results`、`Received`、`Received-SPF` 都是普通的头，
**发件人可以在信里预先写好任意一份**。catch-all 下这不是理论风险。所以认证事实不由 driver 上报，
而由 Go 侧 ingest 按账户配置的可信接收方提取：

- 每个账户配 `trusted_authserv_id`（Cloudflare Email Routing 的、`mx.google.com`、工作邮箱服务器的），
  外部账户的取值是 M3.5 接入时的实测项
- 只采信**最上面一条**、且 authserv-id 匹配的 `Authentication-Results`；其余一律忽略
- 二次 SPF 判定（§3.3、`ix64/spf`）需要的发信方 IP 与 HELO，只从可信接收方自己加的那一行
  `Received`（`by` 是可信主机的最上面一条；它下面的都是发件方一侧写的，不可信）里取；
  `MAIL FROM` 取信封
- 取不到可信接收方的头，就**没有认证事实**：`auth` 记为 `none`，泄漏检测和 agent 视图都按「未验证」处理。
  宁可没有判断，也不要基于伪造输入的判断

这些都是头部解析，在 Go 侧做，不违反「driver 不解析 MIME」。

**Cloudflare Email Routing 的实测（2026-10）**：它在原件最上面依次加上下面这些头，
都在发件人自己写的任何头之上，所以「从上往下取第一条」就是它加的那一条：

| 头 | 内容 |
|---|---|
| `Received` | `from <HELO> (<发信方 IP>) by cloudflare-email.net (cloudflare) id <事务 id> for <RCPT>; <时间>` |
| `ARC-Seal` / `ARC-Message-Signature` / `ARC-Authentication-Results` | `i=1`，`d=cloudflare-email.net` |
| `Received-SPF` | 含 `client-ip`、`helo`、`envelope-from`——二次 SPF 判定需要的三样都在 |
| `Authentication-Results` | authserv-id 为 `mx.cloudflare.net`；含 dmarc、helo 与 mailfrom 两次 spf、`smtp.remote-ip` |
| `X-CF-SpamH-Score` | 垃圾评分，刻度无文档（§4.3） |

所以 Cloudflare 账户的配置是：`trusted_authserv_id = mx.cloudflare.net`，
可信 `Received` 的 `by` 是 `cloudflare-email.net`（两者不是同一个名字）。
`Received` 里的事务 id 在同一笔事务的各个收件人之间相同，发件方重投时会变，
所以它能关联「同一次投递的几路」，不能用来识别重投（§4.1）。

### 10.2 发：SMTP 是通用货币，所以只需要两个 driver

```go
type Transport interface {
    Name() string
    Caps() TransportCaps
    // 入参是我们自己组装好的完整 MIME 字节，已按 Caps 去掉出口不允许调用方设置的头
    Send(ctx context.Context, env Envelope, raw []byte) (*SendResult, error)
}
type SendResult struct {
    Outcome    Outcome            // Accepted | Partial | Rejected | Unknown（DATA 后断开 / 超时）
    Recipients []RecipientResult  // 逐收件人的 SMTP 码，落 outbox_recipients
    ProviderID string
    MessageID  string             // 出口实际使用的 Message-ID；拿不到时为空，见 §10.4.1
}
type TransportCaps struct {
    CallerMessageID  bool  // 能否自定 Message-ID
    CallerReturnPath bool  // 能否自定 Return-Path（VERP 的前提，§10.4.2）
    CallerDKIM       bool  // 能否带自己的 DKIM-Signature（§10.4.4）
    StatusQuery      bool  // 能否按 ProviderID 查投递状态（unknown 对账用）
    // ...
}
```

**关键决策：MIME 由我们自己组装（go-message），transport 只投递字节。**
好处是线程头（`In-Reply-To` / `References`）完全可控，Sent 副本就是我们提交的字节。
但它**不一定等于收件人收到的字节**：有的出口会加上或替换 `Message-ID`、`DKIM-Signature`、
`Return-Path`（Cloudflare 就是，§10.3）。这些差异用 `SendResult` 回填，而不是假装不存在。

由此得出一个让事情简单很多的结论：**优先用 provider 的 SMTP 口，而不是它的
结构化 REST API**。因为 SMTP 是唯一普遍接受 raw MIME 的接口。于是：

- `smtpTransport` 一个实现覆盖：Cloudflare Email Sending、Resend、SES、
  Postmark、Mailgun、Gmail、工作邮箱、任何自建 MTA——差别只是
  `host/port/认证方式(PLAIN vs XOAUTH2)`，是配置不是代码
- 只有 **API-only** 或 API 明显更好的才单独写 driver（Gmail API 的 `messages.send`
  接受 base64url raw，配合它的增量拉取更顺；SES 的 `SendRawEmail` 便于拿 SNS 事件）

#### 10.2.1 Markdown → MIME

撰写用 Markdown（§12.1），所以组装规则要定死。渲染器用 **goldmark**
（Go 生态标准、CommonMark 合规、可扩展）。

完整的 MIME 树（这是实现时最容易搞错的地方）：

```
multipart/mixed
├── multipart/related
│   ├── multipart/alternative
│   │   ├── text/plain   ← Markdown 源码本身
│   │   └── text/html    ← goldmark 渲染 + 样式内联
│   └── image/*  (Content-ID)   ← 正文里引用的内联图
└── application/*  (attachment) ← 普通附件
```

几条硬规则：

- **`text/plain` 直接用 Markdown 源码**。这是选 Markdown 最大的收益：
  纯文本部分天然可读，不需要另写一个"HTML 转纯文本"的降级器（那玩意儿永远写不好）。
- **HTML 必须把样式内联**。邮件客户端普遍剥掉 `<head>` 和 `<style>`（Gmail 就会），
  所以 goldmark 的输出要再过一道 inliner（premailer 思路）。不要外链 CSS，
  不要 `<script>`，表格补 `border`/`cellpadding` 属性给老客户端兜底。
- **`>` 引用在两份里都天然正确**：plain 部分 `>` 就是邮件引用的原生语法，
  html 部分渲染成 `<blockquote>`。Markdown 和邮件在这一点上恰好同源。
- **代码块**不做语法高亮——邮件里 class 样式会被剥掉，`<pre>` 加内联背景色就够。
- **正式邮件的一个权衡**：plain 部分会裸露 `**bold**` 这类标记。
  Markdown 的 plain 可读性本来就是设计目标，日常无妨；但对外发正式邮件
  （如漏洞报告）时略显业余。所以给一个按身份配置的开关：
  plain 部分可选"去标记"渲染（`**bold**` → `bold`），默认保留原文以保真。

### 10.3 能力矩阵

| 出口 | raw MIME | 自定 Message-ID | 任意 From 前缀 | 附件/收件人上限 | bounce 回流 | 计费 |
|---|---|---|---|---|---|---|
| **CF Email Sending (SMTP 465)** | ✓ | ✗ 平台生成，**SMTP 下的表现实测** | ✓ 域级 onboard | 25 MiB / 50 | suppression list + API；不能自定 Return-Path | 3000/月含，$0.35/1k，需 Workers Paid |
| 标准 SMTP submission（工作邮箱/自建） | ✓ | ✓ | 取决服务器策略 | 服务器定 | DSN 回到 Return-Path | — |
| Resend (SMTP 口) | ✓ | 多半 ✓，**实测** | 需验证域 | 40 MB | webhook | 免费额度 + 按量 |
| SES (SMTP 或 SendRawEmail) | ✓ | **可能被改写，实测** | 需验证域 | 40 MB / 50 | SNS 事件（最完整） | 最便宜 |
| Gmail (SMTP 或 API) | ✓ | ✓ | ✗ 只能本人地址/别名 | 25 MB | DSN | 账号配额 |
| 自建 MTA 直投 | ✓ | ✓ | ✓ | 自己定 | DSN | 要养 IP 信誉 |

| 入口 | 形状 | 游标 | 原件完整性 | catch-all | 备注 |
|---|---|---|---|---|---|
| **CF Email Worker** | push | 无（即时） | ✓ 完整 raw | ✓ 原生 | ≤25 MiB，超了 CF 侧就拒，本系统看不到 |
| IMAP (工作邮箱/Gmail) | pull | UIDVALIDITY+UID | ✓ `BODY.PEEK[]` | ✗ | IDLE 可近实时；Gmail All Mail 会重复；卸载对 Gmail 只是去 label（§9.6） |
| Gmail API | pull+push | historyId | ✓ raw | ✗ | 增量最可靠，要 Pub/Sub；`messages.delete` 才真正释放容量 |
| SES inbound / Postmark / Mailgun | push | 无 | ✓ | ✓ | 可作 CF 的替代或并存 |
| POP3 | pull | ✗ 不可靠 | ✓ | ✗ | 没有稳定 UID，只适合一次性导入 |
| Maildir / mbox | pull | mtime | ✓ | — | 历史迁移用 |

### 10.4 抽象漏出的四件事

1. **Message-ID 归属**。能自己生成就自己生成（线程才拼得起来），但这是出口的一项 cap，
   不是默认成立的事。Cloudflare Email Sending 把 `Message-ID` 列为平台控制的头，
   由它用自己的域生成；调用方带了这类头，整封请求会被拒（API 下是 `E_HEADER_NOT_ALLOWED`，
   SMTP 下是拒收还是改写，文档没写，要实测）。所以：
   - `CallerMessageID=false` 的出口，组装时**不写** `Message-ID`，发出后把出口实际用的那个
     回填到 `outbox.sent_message_id`。线程匹配同时认我们自己的 id 和回填的 id，
     否则对方回信的 `In-Reply-To` 对不上本地任何一条
   - 回填从哪里拿是实测项：SMTP 的 `250` 响应文本、事件订阅、Activity log。
     Cloudflare 的 Worker send binding 会返回一个 `messageId`，它是否等于 `Message-ID` 头也待实测；
     若 SMTP 拿不到而它能，CF 出口改走 Worker binding，多写一个 driver
   - 都拿不到时，靠对方回信 `References` 里我们自己写进去的上游 id 拼线程；
     只有一条新线程的第一封会断开，落到 `class=human` 的 `subject + sender` 兜底匹配（§4.1）

   **接入任何新出口的第一个测试：发一封给自己，比对 Sent 副本和收到的字节**，
   把差异逐项落进 `TransportCaps`。

2. **bounce / DSN 回流路径完全不同**（CF 的 suppression list、SES 的 SNS、
   SMTP 的 DSN 邮件）。统一成一个 `DeliveryEvent{outbox_id, kind, at, detail}`
   喂回 outbox 状态机（§3.6）。注意 DSN 是**一封邮件**，会从入口 driver 进来——
   所以 ingest 流水线要能识别 `multipart/report` 并路由到 outbox 而不是收件箱。
   这是收发两侧唯一的耦合点，值得显式写出来。
   DSN 识别本身也是攻击面：catch-all 下任何人都能投一封伪造的 `multipart/report`，
   把一封已送达的信标成退信、甚至往 suppression 里塞地址。所以 `Return-Path` 的本地部分
   要嵌 outbox id（VERP：`bounce+<outbox_ulid>@example.com`，存 `outbox.verp_tag`），
   DSN 只有在 `envelope_to` 的 tag 对上一条 outbox 记录时才喂回状态机，对不上的照常进收件箱。
   能否自定 Return-Path 是 transport 的一项 cap（`CallerReturnPath`）。
   Cloudflare 把 `Return-Path` 列为平台控制、指向它自己的退信处理器，所以 **CF 出口没有 VERP**：
   只依赖它的 suppression list 和事件，该出口的 DSN 路径关闭，伪造 DSN 也就无从喂回状态机。

3. **能力不对等会漏到 UI 和 agent 面**。`SourceCaps/TransportCaps` 必须显式，
   UI 按 caps 灰掉做不到的操作（POP3 账户没有"标记已读回写"、
   `metadata` 策略的账户不能全文搜、Gmail 的 label 不是 folder），
   而不是假装都一样然后运行时报错。**抽象要诚实**。

4. **出口可替换的真正成本在 DNS，不在代码。**
   想要哪天能切到备用出口，SPF 的 `include` 和 DKIM selector 必须**提前**
   给所有候选出口配好。否则"替换"那天要等 DNS 生效，而且期间的信大概率进垃圾箱。
   建议现在就把 CF + 一家备用（SES 最便宜）同时授权，`transport chain`
   支持 primary + fallback 才有意义。
   既然 MIME 由我们组装（§10.2），在 `CallerDKIM=true` 的出口上 **DKIM 可以在 Go 侧自签**：
   自有 selector、私钥在自己机器上，`d=` 对齐 From 域，DMARC 不依赖 provider 的签名。
   但 Cloudflare 把 `DKIM-Signature` 也列为平台控制，自签的信会被整封拒收——
   **CF 出口只能用它自己的签名**。所以「出口可替换」的 DNS 成本是：
   CF 的 DKIM 记录 + 每个备用出口的 DKIM（或自签 selector）+ 各自的 SPF include，都要**提前**配好。
   入口侧对应的成本是 MX 切换：catch-all 的 MX 只能指一家，迁移期双收靠
   `(message_id, body_hash)` 折叠兜住（§4.1 的机制正好复用）。

### 10.5 一致性测试

driver 层做**契约测试**：一组 `.eml` 语料（多部分、中文编码、
超长 header、畸形 MIME、DSN、带 inline 图）+ 一套契约断言
（幂等、游标可续、raw 字节不变、envelope 正确提取）。
**任何新 driver 跑通这套就能接入**——这是 driver 可替换性唯一的真实保证，
不是接口声明。

---
