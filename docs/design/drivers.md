# 收发 Driver 契约与能力矩阵

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 10. Driver 契约与能力矩阵

**是的，收和发都是 driver，但"可替换"要说清楚边界。**

能替换的根本原因只有一个：**RFC 5322 的字节流是共同货币**。所有 driver 的职责
都压缩成"把 raw MIME 搬进来 / 搬出去"，解析、线程、分类、存储只有一份实现。
driver **一律不解析 MIME**——这是契约里最重要的一条。

漏出抽象的四件事（§10.4 细说）：Message-ID 归属、bounce 回流路径、
能力不对等、DNS 预授权。这四件不认真处理，driver 抽象就是假的。

### 10.1 收：Push 和 Pull 两种形状，一个产出

**实现现状**：下面的 driver 接口还没有落成代码。目前只有 Cloudflare 一个 push 来源，
它直接实现在 [`internal/ingest`](../../internal/ingest/ingest.go)（§2.1 的 Go 侧），
写库入口是 [`store.CommitIngest`](../../internal/store/store.go)，`source_key` 由 `store.PushSourceKey` 算出。
接口和 `InboundRef` 仍是设计，等第二种来源（IMAP，M3.5）接入时再从现有代码里抽出来。
IMAP 拉取的形状已定（§10.1.3），存储一步已落地：写库入口是和 `CommitIngest` 并列的 [`store.CommitPull`](../../internal/store/pull.go)，不复用 `PushSourceKey`，
三种拉取键的写法也在那里；同步引擎还没有；
IMAP 协议由自己写的只读客户端 `internal/imap` 说（§5）。下面的接口是目标形状，抽的时候以两个实现的实际需要为准。

```go
// 所有 driver 的唯一产出：原件引用 + 稳定标识 + 远端定位 + 来源
type InboundRef struct {
    AccountID    AccountID
    SourceKey    string       // 来源处的稳定标识，账户内唯一，即幂等键；push 来源留空，由 ingest 填（见要点）
    Loc          RemoteLoc    // 远端定位；push 来源和 Gmail API 为零值
    RawKey       string       // 已落 RawStore 的 key；Completeness=None 时为空
    Completeness Completeness // Full | HeaderOnly | None，由账户的 sync_policy 决定（§9.2）
    Header       []byte       // 仅 None：只在内存里用于生成通知，不落盘
    Size         int64
    SHA256       [32]byte
    EnvelopeTo   []string     // RCPT TO，catch-all 判前缀靠它而不是 To 头；每个 RCPT 一条 delivery
    EnvelopeFrom string
    ReceivedAt   time.Time
}

// IMAP 的 UID 只在 (Mailbox, UIDValidity) 之内有意义，三者一起才是一个定位。
// 定位会变（UIDVALIDITY 重置、信被移到别的文件夹），所以它不是标识。
type RemoteLoc struct {
    Mailbox     string
    UIDValidity uint32
    UID         uint32
}

// 对远端单封信做操作时传的完整引用：Loc 用来找到它，SourceKey 用来确认找到的就是它
type RemoteRef struct {
    SourceKey string
    Loc       RemoteLoc
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
    FetchBody(ctx context.Context, ref RemoteRef) (io.ReadCloser, error)
}

// 可选：把本地的已读/归档写回远端（§9.2 默认关）
type FlagWriter interface {
    SetFlags(ctx context.Context, ref RemoteRef, f Flags) error
}

// 可选：归档卸载（§9.6）。唯一不可逆的远端动作，只由系统批处理调用，没有 API 暴露它。
// 在同一个会话里按 ref 重拉原件、sha256 等于 want 才删；任何一步对不上就什么都不做。
// Caps.ExpungeSemantics 说明"删了之后容量什么时候真正释放"（IMAP 立即 / Gmail 进 Trash 30 天 / 不支持）
type Expunger interface {
    Expunge(ctx context.Context, ref RemoteRef, want [32]byte) error
}
```

要点：
- **Push 的 ack 语义**：`Accept` 返回成功才算收下。Cloudflare Worker 侧只要
  R2 写成功就 ack，所以自建服务离线不丢信（§3.5）。
- **Pull 的游标**：`Cursor` 对 IMAP 是每个文件夹一份 `(UIDVALIDITY, lastUID)`，
  对 Gmail API 是 `historyId`，对 Maildir 是 mtime+inode。
- **稳定标识和定位是两件事**。`SourceKey` 回答「是不是同一封」，`RemoteLoc` 回答「现在去哪里找它」。
  前者做幂等键，后者做操作；两者混用，要么幂等失效，要么操作落到别的信上。
- **幂等**：`unique(account_id, source_key)` 在 `deliveries` 上（§4），driver 不需要自己记"处理过没有"。
  `SourceKey` 的取值按来源能给出的最稳定的东西：

  | 来源（文件夹的键类型） | `SourceKey` | 跨文件夹移动 | 跨 UIDVALIDITY 重置 |
  |---|---|---|---|
  | Gmail（服务器宣告 `X-GM-EXT-1`，预设 `gmail`） | `gm:<X-GM-MSGID 的 16 位小写十六进制>` | 稳定 | 稳定 |
  | 宣告 OBJECTID（RFC 8474）的 IMAP | `oid:<EMAILID>` | 稳定 | 稳定 |
  | 其他 IMAP | `loc:<uidvalidity>:<uid>:<mailbox>` | 变 | 变（§10.1.3） |
  | push 来源（Worker 等） | `content_hash` + `envelope_to`（`store.PushSourceKey`），ingest 算完 `content_hash` 后填 | — | — |

  拉取来源的三种格式 M3.5 已定，**是单向门**：`(account_id, source_key)` 是幂等键，一个账户入库之后键的写法就不能改，
  改了整个账户会以新键重新入库一遍。几处取舍：
  - 键带种类前缀，同一账户里不同来源、不同种类的键不会撞
  - `loc:` 把 mailbox 放在最后：文件夹名里可以有任何分隔符，放最后就不必转义，前两段是数字，切分没有歧义
  - **键类型按文件夹在首次同步时固定**（记在 `source_mailboxes.key_kind`，§4）。服务器哪天开了 OBJECTID，
    不自动从 `loc:` 换成 `oid:`，否则整个文件夹会以新键再入库一遍：投递重复（内容层仍折叠），定位也乱了。
    要换就走一次显式的重扫，以后再做
  - Gmail 用十六进制而不是十进制，是因为 Gmail API 的 message id **据说**就是 `X-GM-MSGID` 的十六进制（待实测，§10.1.5）。
    是的话，以后同一个账户换成 Gmail API driver 时键直接接上；不是也只是那时多一次映射，键的写法不受影响

  push 来源用内容而不是 Worker 的调用 id，是因为调用 id 每次重投都会变：用它做键，重投去重就只能先查后插。
  用收件地址加内容哈希做键，重投直接撞上唯一约束（§4.1）。调用 id 仍然是 `raw_objects.id`。
- **对远端的每个操作都先验证定位**（`FetchBody` / `SetFlags` / `Expunge` 一样）：
  1. `SELECT` 后 UIDVALIDITY 与 `Loc` 不同 → 不动作，返回 `ErrStaleLoc`
  2. `SourceKey` 是稳定 id 的来源，再 `UID FETCH <uid> (X-GM-MSGID)`（或 `EMAILID`）确认对得上
  3. 动作只按 UID：`UID FETCH`、`UID STORE`、`UID EXPUNGE`（UIDPLUS，RFC 4315）或 `UID MOVE`（RFC 6851）。
     不用普通 `EXPUNGE`：它会删掉该文件夹里所有带 `\Deleted` 的信，包括别的客户端标的。
     服务器两者都不支持，`Caps` 里就没有 `Expunger`

  `ErrStaleLoc` 由同步任务重新解析定位后再试，不在调用点猜。同一 UIDVALIDITY 下一个 UID 对应的内容不可变
  （IMAP 的保证），所以验证通过之后在同一个会话里的动作作用的一定是验证过的那封。
- **定位失效时**：有稳定 id 的来源按 id 找回当前 UID（Gmail 是 `UID SEARCH X-GM-MSGID <id>`，
  OBJECTID 是 `UID SEARCH EMAILID <id>`），只改写定位，`source_key` 不变。
  其他 IMAP 只能在 UIDVALIDITY 变化后全量重扫该文件夹，按 raw sha256 对上同一账户同一文件夹里已有的
  delivery，**原地改写**它的定位和 `source_key`，而不是插入新行（步骤见 §10.1.3）。
- **Gmail 走 IMAP 时只同步 All Mail**：按 SPECIAL-USE 的 `\All`（RFC 6154）找，不写死 `[Gmail]/All Mail`——
  文件夹名随界面语言变。label 用 `X-GM-LABELS` 取，存进 `deliveries.remote_labels`（拉取时的快照）。否则同一封信在 INBOX 和 All Mail 里各算一次投递。
  All Mail 不含 Spam 和 Trash、含不含草稿，都待实测（§10.1.5）。其他服务器默认同步所有可选的文件夹，去掉 `\Junk`、`\Trash`、`\Drafts`，可按来源配置
- **Gmail 的 `\Sent` 照常入库、从不推送**。经 `smtp.gmail.com` 或别的客户端发出的信都会出现在 All Mail 里：它是这个邮箱里的事实，
  在别的客户端里发的信也只有这样才进得来。经我们自己的出口发出的那份多半和 Sent 副本折叠（§4.6）。推送一律不发：那是自己写的信
- **带 `\Draft` 标志的信不入库，只计数**，计数写进同步状态，`account ls` 和界面看得见。这是整个系统里**唯一看到了但不收的信**。
  理由：草稿每保存一次，服务器上就是一封新信、一个新 UID，入库就是一串互相折叠不了的半成品版本，没有一份是发生过的通信；
  真发出去的那份会带着 `\Sent` 回来。代价是 yuuka 里看不到在别的客户端里写了一半的信，那本来就不是收件箱该管的
- **`metadata` / `notify` 策略不是"拉下来再丢"**：
  - `metadata`：driver 用 `BODY.PEEK[HEADER]` 只拉头部，`Completeness=HeaderOnly`，
    头部照常落 RawStore，解析器据此只建头部索引；点开时走 `FetchBody` 实时取（§4.4）
  - `notify`：`Completeness=None`，头部只在内存里生成一条通知，不落 RawStore、不建 message；
    `deliveries` 只留一行幂等用的桩
  否则 InboundRef「已落 RawStore」的契约和 §9.2「正文不落盘」直接矛盾。

#### 10.1.1 认证事实只信可信接收方

`Authentication-Results`、`Received`、`Received-SPF` 都是普通的头，
**发件人可以在信里预先写好任意一份**。catch-all 下这不是理论风险。所以认证事实不由 driver 上报，
而由 Go 侧 ingest 按账户配置的可信接收方提取（实现见 [`internal/authres`](../../internal/authres/authres.go)）：

- 每个账户配 `trusted_authserv_id`（Cloudflare Email Routing 的、Gmail 的、工作邮箱服务器的）。
  Cloudflare 账户的那一个留在主机配置（§17.8）；拉取账户的在 `account_sources` 上，**默认为空，空就是不采信任何一条**，
  所有信「未验证」。Gmail 预设建议填 `mx.google.com`，但它是不是 Gmail 交出来的原件里**最上面一条** `Authentication-Results`
  的 authserv-id 待实测（§10.1.5），测了再写进预设；工作邮箱逐个实测，测不出来就留空。预设只是 `account add` 时填的默认值，
  库里存的是值本身
- **按投递所属的账户取**：内容步骤（折叠）、补跑和 reparse 都用这条投递的账户的值
  （[`ingest`](../../internal/ingest/content.go) 的 `authservIDs`）：有拉取来源的账户取 `account_sources` 上的，其余的是 Cloudflare 账户，取主机的。
  同一份内容经 Cloudflare 和拉取的邮箱各进来一次时，两条投递各按自己的接收方判断；只用一个值，reparse 就会拿 Cloudflare 的 authserv-id 去读 Gmail 的信，结论全是「未验证」（§4.2）。
  改它只对之后入库的信生效，已有投递要 reparse；改它算放宽（§13.10），包括清空——清空让所有信「未验证」，看似收紧，
  却会让可信接收方报了 `dmarc=fail` 的信不再降级
- 只采信**最上面一条**、且 authserv-id 匹配的 `Authentication-Results`；其余一律忽略
- 可见 `From` 是不是真的，只看这条可信 `Authentication-Results` 里的 **dmarc**（§3.3）。
  spf、dkim 的结果作为辅助证据一并记下，但 SPF 验证的是信封 `MAIL FROM` 域或 HELO，不是信头 `From`
- 辅助的二次 SPF 判定（`ix64/spf`）需要的发信方 IP 与 HELO，只从可信接收方自己加的那一行
  `Received`（`by` 是可信主机的最上面一条；它下面的都是发件方一侧写的，不可信）里取；
  `MAIL FROM` 取信封。它在 ingest 时算一次，结果连同查询时间存进 `deliveries.spf_recheck`，
  不覆盖可信接收方的结论，reparse 也不重算（§4.2）
- 取不到可信接收方的头，就**没有认证事实**：`auth` 记为 `{"trusted": false, "reason": …}`，
  不带任何结果，泄漏检测和 agent 视图都按「未验证」处理。最上面一条的 authserv-id 不是可信的那个，
  同样按未验证处理，不往下找——下面的都可能是发件人写的。宁可没有判断，也不要基于伪造输入的判断
- `X-CF-SpamH-Score` 只从原件最顶上那一段接收方字段里取（`Received` 只算第一条，
  其余是 `ARC-*`、`Received-SPF`、`Authentication-Results`、它自己）；同名字段出现在更下面就是发件人写的，忽略。
  Cloudflare 没加而发件人恰好把它写在自己头部第一行时分辨不出，它只是信号、不单独决定任何事，可以接受
- Cloudflare 账户的 `trusted_authserv_id` 是进程配置（见 [`cmd/yuuka/config.go`](../../cmd/yuuka/config.go)，
  默认是下文实测的 Cloudflare 取值），一个进程对应一个 Cloudflare 账户。`spf_recheck` 仍恒为空，二次 SPF 判定还没接

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

正文末尾还会多出一个 CRLF（多一个空行），2026-10-31 实测：经 Email Sending 和直投 MX 的两种发件方都有，
标记里的 `raw_size` 等于对象长度，说明 Worker 读到的 `message.raw` 里就已经有它，是 Cloudflare 入站一侧加的；
Worker 原样写入，不去掉——`raw/` 的含义是「Cloudflare 交给 Worker 的字节」。它不影响 DKIM
（两种正文规范化都忽略末尾空行）、检索文本和 multipart（落在 epilogue）；会让同一封信经不同路径进来时
`content_hash` 不同。M3.5 打算在折叠时容忍这一个 CRLF，前提是字节比对证实它是唯一的差异，否则按 §4.1「宁可重复显示、不可误合」接受（§4.1）。

所以 Cloudflare 账户的配置是：`trusted_authserv_id = mx.cloudflare.net`，
可信 `Received` 的 `by` 是 `cloudflare-email.net`（两者不是同一个名字）。
`Received` 里的事务 id 在同一笔事务的各个收件人之间相同，发件方重投时会变，
所以它能关联「同一次投递的几路」，不能用来识别重投（§4.1）。

#### 10.1.2 IMAP 拉取：服务器说的话不能全信

M3.5 还没有落地，下面是落地时的要求；一轮拉取的具体步骤和提交顺序见 §10.1.3。来源是其他 IMAP 客户端在真实服务器上踩过的坑
（主要是 gofer 的同步引擎，§1.2），按本项目的不变量改过：gofer 是缓存，可以清掉重拉；
我们是权威存储，远端的任何异常都只能改投递的定位和状态，不能删内容。

**变化检测按代价分级**，大多数轮次应当停在第一级：

1. 有 LIST-STATUS（RFC 5819）就一条命令拿全部文件夹的 `UIDVALIDITY` / `UIDNEXT` / `MESSAGES`
   （有 CONDSTORE 再加 `HIGHESTMODSEQ`），和上一轮一样就不打开这个文件夹；没有就逐个 `STATUS`
2. 新信按 `UID FETCH <lastUID+1>:<UIDNEXT-1>` 取，上界取本轮开头看到的 `UIDNEXT`。
   不写 `<lastUID+1>:*`：RFC 3501 规定 `*` 是最后一封信的 UID，没有新信时这个范围会回头包含最后一封
3. 游标在本轮成功之后才写。本轮进行中到达的信 UID 不小于开头的 `UIDNEXT`，下一轮自然补上，不需要锁
4. 标志变化只在卸载要看 `\Seen`（§9.6）或开了回写（§9.2）时才取：有 CONDSTORE 用 `CHANGEDSINCE`；
   服务器声明了 CONDSTORE 却拒绝这条命令时，退回分块 `FETCH FLAGS`；`HIGHESTMODSEQ` 倒退时丢掉检查点重建

**服务器的回答要交叉核对**：

- `UID SEARCH ALL` 的结果数和 `EXISTS` 对不上（有的服务器返回空或不全），退回 `UID FETCH 1:* (UID)` 列 UID，
  不能拿不全的列表去判断哪些信消失了
- 有的服务器报的 `UIDNEXT` 偏小。判定消失只看 `max(UIDNEXT, 已见最大 UID + 1)` 以下的 UID
- 拉取一律用 `EXAMINE` 打开文件夹（只读，副作用再小也不要）。换文件夹时服务器声明了 `UNSELECT` 才用它，
  否则直接 `EXAMINE` 下一个；有服务器连续拒绝几条命令就断开连接。绝不用 `CLOSE`：在读写打开的文件夹上它会顺手 expunge

**消失要确认多次才算**。远端列表里少了一个 UID、取信返回不存在、文件夹从 `LIST` 里不见了，
都可能是服务器的瞬时状态。连续几轮（次数可配置）都观察到才把投递记为远端已消失，文件夹另给一段宽限期；
消失了的文件夹再出现，按 UIDVALIDITY 判断是不是原来那个。无论哪种都只改 `deliveries.remote_state`，
不删投递、不删内容（§9.6）。

**单封超限不能静默跳过**。拉取前从 `RFC822.SIZE` 就知道大小，上限可配置；超限的信按 `HeaderOnly` 落头部
（§4.4），投递标「超限，原件未取」，界面里看得见，点开或人放行时走 `FetchBody`。
服务器发来的 literal 长度超过上限时，在读它之前就断开，不把它读进内存。

**首次同步从新到旧**。人最先要的是最近的信，旧信可以慢慢补。所以每个文件夹的游标是
`(UIDVALIDITY, high, low)`：`high` 往上追新信，`low` 往下回填，回填中断了下次从 `low` 续跑。
两段的幂等都靠 `(account_id, source_key)`（§10.1），重叠无害。

**容量和限速**：

- 每个服务器主机限制并发连接数，已登录的会话复用，空闲时 `NOOP` 保活
- 每批取几百封，下载下一批和写入上一批流水线重叠
- `Retry-After` 秒数和 HTTP 日期两种写法都要解析，并设上限
- Gmail 对 IMAP 有每账户的连接数和每日下载量上限，大邮箱的首次全量拉取会撞上。撞上就停，第二天从游标续跑，
  而不是报错重试。具体数字（印象是 15 条连接、每天 2500 MB）和撞限时的回应形状都待实测（§10.1.5）
- 落地前先写一个 10^5 封量级的大邮箱基准（首次同步和空轮询各一个），把轮次耗时和内存定下来

**以后接 Gmail API driver 时**：每个邮箱一个令牌桶，目标留在官方配额的八成左右，给交互请求留余量，
429 时速率减半再慢慢恢复；`history.list` 的 `historyId` 过期（404）时从当前 `historyId` 重新开始，
并按标签补拉最近几页，补漏靠幂等键兜底。

#### 10.1.3 IMAP 拉取的一轮、提交顺序与不丢信

> 状态：M3.5，已定。存储一步已落地：表在 [0019](../../internal/store/migrations/0019_pull_accounts.sql)，
> 提交、游标的 CAS、失败表、重映射和 worker 的锁在 [`internal/store/pull.go`](../../internal/store/pull.go)（取舍见 §4）；同步引擎未实现。

**谁在拉**：`serve` 里一个管理协程按 `account_sources.revision` 和机密的 revision 启停每个来源的 worker，
和渠道的管理协程（§17.5）同一个形状，定期读表，不做重载信号。一个启用、未暂停的来源一个 worker，每个账户同一时刻一条连接。
跑多个 `serve` 时，worker 先拿这个账户的会话级 advisory lock（`pg_try_advisory_lock`，键由账户 id 派生，进锁顺序的测试），
拿不到就这一轮不干。没有 KEK、来源的机密解不开时 worker 不启动，Cloudflare 收信不受影响（§17.3）：远端还保有信，这是延迟，不是丢信。

**一轮**（单个账户）：

1. 连接（只做隐式 TLS）、`CAPABILITY`、`ID`、登录。`ID` 是不是 Gmail 必需的待实测（§10.1.5），发了无害
2. `LIST`：有 `LIST-STATUS` 就带上 `UIDVALIDITY UIDNEXT MESSAGES`，有 `SPECIAL-USE` 就带上它。要同步哪些文件夹见 §10.1
3. 和上一轮看到的 `UIDVALIDITY / UIDNEXT / MESSAGES` 一样的文件夹跳过（§10.1.2 第一级）
4. `EXAMINE`。UIDVALIDITY 变了走下文的重映射
5. **追新**：`UID FETCH <high+1>:<本轮开头的 UIDNEXT - 1> (UID RFC822.SIZE INTERNALDATE FLAGS [X-GM-MSGID X-GM-LABELS | EMAILID])`。
   先为每封算出 `source_key`，**一条查询筛掉库里已有的**，只对剩下的取 `BODY.PEEK[]`
6. 大小超过 `max_message_bytes` 的改取 `BODY.PEEK[HEADER]`，按只有头部入库，标 `remote_omitted`（§4.4）。
   literal 的长度在读之前检查，超了就断开，不读进内存：任何时刻内存里至多一封 `max_message_bytes` 的信
7. 每封按下面的顺序落盘、提交。一批（默认 100 封或 20 MiB）都提交之后，在事务外用 CAS 推进游标：
   只在 `uidvalidity` 仍是读的那个、且新值更大时写
8. **回填**：`low` 往下，`UID FETCH <low-N>:<low-1>`，从新到旧；和追新交替，追新优先（§10.1.4）
9. `LOGOUT`，按 `poll_seconds` 等下一轮。先轮询（默认 60 秒一次 `STATUS` 级别的检查，几乎不花钱），IDLE 等轮询跑稳再加：
   它要每个邮箱常驻一条连接、处理服务器踢线，换来的是验证码早到不到一分钟

**落盘与提交**，顺序和 Sent 副本（[`outbox.StoreSent`](../../internal/outbox/outbox.go)）相同，再多一个事务：

1. `raw_id` 是新的 ULID，key 是 `pull/YYYY/MM/<raw_id>.eml`（§2.2）
2. 先写本地副本（原子、不可覆盖），再写 R2（[`RawBucket.PutPull`](../../internal/r2/pull.go)）：条件写，412 时它自己读回比对字节和 metadata，
   相同即成功；对象带 `yuuka-pull`（§2.2）。metadata 编码失败（字段非 UTF-8、超过上限）算这一封取不下来，按下文进 `pull_failures`
3. 一个事务 `store.CommitPull`：`raw_objects`（`received_at` 取 `INTERNALDATE`）加 `deliveries`，
   投递按 `(account_id, source_key)` 插入，撞了就和 push 一样只记 `duplicate_of`、不建第二条
4. 提交之后跑内容步骤（解析、折叠、线程、规则），可信接收方取这个账户的（§10.1.1）；推送按 §10.1.4
5. 游标按上面第 7 步推进

**§3.5 对拉取的含义**：在卸载之前远端一直保有原件，所以「不丢」就是**游标永远不越过一封没提交的信**：

| 崩溃在 | 结果 |
|---|---|
| 写本地或 R2 之前 | 游标没动，下一轮重拉 |
| R2 写完、事务之前 | 留下一个没有行的 `pull/` 对象，无害；下一轮重拉，新 id、新对象 |
| 事务之后、游标之前 | 下一轮追新时按 `source_key` 的筛选直接跳过它；并发时也只是撞唯一约束 |
| 内容步骤失败 | 投递已提交，由现有的补跑（`ProcessBacklog`）收拾，和 push 相同 |

**一直取不下来的信**（服务器对它回 `NO`、literal 截断、`yuuka-pull` 编码不出来）：同一封连续失败若干次（默认 5）后写一行 `pull_failures`，游标越过它；
`account ls` 和界面显示失败计数，`account sync --retry-failed` 重试。不越过，整个文件夹永远卡在这一封；
越过而不留记录，就是静默丢信。记下来越过，两害取其轻，而且看得见。

**拉取这边的 reconcile**：

- 每轮的追新加回填本身就是对账：缺口按游标补，幂等键兜底
- 每周一次全量对账，和 Cloudflare 一侧的全量扫描（[`FullScan`](../../internal/ingest/reconcile.go)，§2.1 对账表的最后一行）同一个节奏：每个文件夹 `UID SEARCH ALL`
  （结果数和 `EXISTS` 对不上就退回 `UID FETCH 1:* (UID)`，§10.1.2），`high` 以下、库里没有对应投递、也不在 `pull_failures` 里的 UID 重新拉。
  防的是 `UIDNEXT` 报小、服务器瞬时少给这一类让游标越过了信的情况
- `pull/` 下没有行的对象只计数报告，不删：`RawBucket` 没有删除方法，也不该有（§2.2）

**UIDVALIDITY 变了**：

- **Gmail、OBJECTID**：键不变。对整个文件夹取 `UID FETCH 1:* (UID X-GM-MSGID)`（或 `EMAILID`），按键原地改写已有投递的定位（§10.1「只改写定位」），
  没见过的照常入库。不下载正文
- **其他服务器**：只能全量重扫。逐封下载、算 sha256，对上「同一账户、同一文件夹、旧 UIDVALIDITY」下的投递，就**原地改写它的定位和 `source_key`**，
  下载的字节丢掉，不存第二份原件；对不上的按新信入库。整个文件夹要重下一遍，按日预算慢慢跑；跑完之前追新照常，
  新 UID 用新 UIDVALIDITY 的 `loc:` 键，`source_mailboxes.rescan_uidvalidity` 记着还有旧投递没对完。
  这里假设服务器两次交出同样的字节，待实测：有的服务器会重新生成头部，对不上的结果是重复投递（内容层仍可能折叠），不是丢信
- 改写定位和 `source_key` 是在改投递事实，所以只在这一条路径里做，在同一个事务里带着旧值做 CAS

**限速与失败**：

- 每个账户一条连接；同一个主机上的所有账户共用一个并发上限（默认 2）
- 日字节预算：Gmail 预设默认 2 GiB / 天，低于 Gmail 的下载上限（数字待实测）；其他服务器默认不限。
  撞到服务器的限额就停到第二天、从游标续跑，不报错重试（§10.1.2）
- 连接、TLS、超时一类错误指数退避，最长 1 小时
- **登录被拒**（`[AUTHENTICATIONFAILED]`）不重试：来源进暂停（`pause_reason = auth_failed`），`account ls` 和界面显示原因，
  日志每天报一次 ERROR，直到人改了密码或 `resume`。反复用错的密码登录 Gmail 会被锁，重试只会让事情更糟。
  显示的是原因，不是服务器的原话：那是不可信输入
- 落地前写 §10.1.2 要求的 10^5 封基准（首次同步、空轮询），把轮次耗时和内存定下来

**`yuuka account sync` 只是请 `serve` 跑一轮**：它在库里写一个请求时间，worker 看到就跑，CLI 进程自己从不拉信。
CLI 没有 worker 的 advisory lock 和限速状态，在里面拉会和 `serve` 抢连接数和日预算。代价是没跑 `serve` 时它不起作用，命令要把这一点说清楚。

#### 10.1.4 回填与拉取的副作用

> 状态：M3.5，已定，未实现。

**首次同步回填全部，从新到旧，按日预算走**（Gmail 默认 2 GiB / 天）。可以按来源改成不回填或只回填最近若干天。
默认全部，是因为「一个收件箱看全部」（§9）打了折就不是统一视图，M3.6 的卸载也要求 yuuka 有全部原件。
放弃的是另一种默认「只回填 90 天」：省下载和存储，代价是旧信要人自己记得去补。

补跑、通知、评分原先都假设「新入库 = 新到达」。拉取打破了它：Gmail 首次同步十万封，不加区分就是十万条推送、十万次评分请求。所以：

| | 追新（`high` 往上，且首次同步已经追平） | 回填（`low` 往下）、首次同步 |
|---|---|---|
| 规则 | 跑 | 跑：规则是「到达时的决定」（§8.5），回填的信对 yuuka 来说就是现在到达；规则只做可撤销的标注 |
| 推送 | 照现有的降级判断（§4.3），另加三条：`INTERNALDATE` 在 24 小时内；Gmail 标签不含 `\Sent`；不是我们自己发出的信（有可见的 outbox 行） | **从不** |
| 评分（§15） | 只在主机允许这个账户评分时（见下） | **从不**；评分器自己按回溯期补评另说 |
| `changes` | 照写 | 照写：和 0007 的补跑一样会有一波事件，可以接受 |

**拉取账户评不评分由主机决定**：新环境变量 `YUUKA_TRIAGE_ACCOUNTS`（随代码落地），默认只有 Cloudflare 账户。
评分把信的内容交给评分器，可能是第三方，这是数据出境的决定，按 §17.1 属于主机，不属于一次可能被劫持的会话；
把 Gmail 的全部历史默认交出去，不是接一个账户的人预期的事。

**拉取投递的 zone / prefix 是邮箱自己的地址**，`account add` 顺带认领它（§9.3）。不认领，§4.3 的降级会把这个邮箱的每一封信都降掉：
降级要求前缀已认领。另一种做法是让拉取账户的投递不看前缀状态，但那个条件拼在十来处 SQL 里，还要把内连改成外连；
认领则让降级、泄漏检测、回复身份一行不改地成立，代价只是前缀列表里多一行外部地址。

#### 10.1.5 M3.5 落地前的实测项

下面这些在设计里用到了，但只是推测，落地前用真实账户测过再写死。测出来不一样时，受影响的设计写在括号里：

- Gmail 交出来的原件里，最上面一条 `Authentication-Results` 的 authserv-id 是不是 `mx.google.com`；
  Gmail 在它上面加的 `Delivered-To`、`Received`、`X-Received`、`ARC-*` 不影响「最上面一条」（预设的建议值，§10.1.1）
- 同一封信经 Cloudflare 和 Gmail 两路进来，去掉 v1 剔除的头之后是否**只差末尾一个 CRLF**；
  经 Gmail SMTP 发出、又从 All Mail 拉回的信能不能和 Sent 副本折叠（跨账户折叠的容差，§4.1、§4.6）
- All Mail 含不含草稿；是否排除 Spam 和 Trash（`\Draft` 的处理、只同步 `\All`，§10.1）
- Gmail IMAP 的限额：每账户并发连接数、每日下载量、撞限时的回应形状（如 `[OVERQUOTA]`、`BYE` 带带宽字样）（日预算和停到第二天，§10.1.3）
- `X-GM-MSGID` 的十六进制是不是 Gmail API 的 message id（`gm:` 键的写法，§10.1）
- Gmail 是否要求客户端发 `ID`（§10.1.3）
- Gmail 个人账户的 app password 是否仍可用（要求两步验证，Workspace 管理员可以禁用）。Google 一直在收紧，被单方面关掉时只能走 OAuth（M3.5b）
- UIDVALIDITY 重置后非 Gmail 服务器是否两次交出同样的字节（定位键的重扫，§10.1.3）
- 10^5 封邮箱的首次同步耗时和内存（§10.1.2）

M3.5b 的另有一组，见 §17.8。

### 10.2 发：SMTP 是通用货币，所以只需要两个 driver

SMTP 出口实现在 [`internal/transport`](../../internal/transport/smtp.go)：`SMTP.Send(ctx, Envelope, raw, commit)`
返回 `SendResult`（`Outcome`、逐收件人结果、`Committed`、最终回复原文、`ProviderID`、`MessageID`），
出口能力是配置里的 [`Caps`](../../internal/transport/transport.go)。目前只有这一个实现，
所以没有 `Transport` 接口；outbox 在消费方按它用到的方法（`Caps`、`Send`）定义一个小接口（[`outbox.Transport`](../../internal/outbox/outbox.go)），
第二个 driver（API 型出口）出现时形状不变。

**提交点**。`commit` 恰好调用一次，时机是正文全部写完并 flush 到连接之后、正文结束符 `<CRLF>.<CRLF>` 之前；
调用方在里面写 `committed_at` 并提交事务（§4.5）。`commit` 返回错误时直接关连接、不发结束符——
服务器只在收到结束符时处理这封信，之前断开就丢弃。这个保证依赖 `net/textproto` 的 `DotWriter`：
它写进 `bufio.Writer`，结束符只在 `Close` 里写出，所以「写完 + flush」之后线上有全部正文而没有结束符。
陷阱也在这里：`textproto` 在写**任何**命令之前会先关闭打开着的 `DotWriter`，也就是发出结束符，
所以放弃时连 `QUIT` 都不能发，只能关连接。`net/smtp` 的 `Data()` 把最终回复的文本吞掉了，
所以 `MAIL` / `RCPT` / `DATA` 和最终回复直接走 `textproto`；问候、`EHLO`、`AUTH PLAIN` 仍用 `net/smtp`。
回调之前把正文 flush 出去还有一个好处：连接已断这种失败在提交点之前暴露，仍然是可以安全重试的失败。
本来没送达、只因 `commit` 之后被取消或超时而判成 `unknown`，是 §4.5 接受的唯一误判方向。

**结果分类**。每个收件人一个结论，`Outcome` 只由逐收件人结论推出，两者不会矛盾：

| 发生了什么 | 收件人结论 | `Outcome` |
|---|---|---|
| 本地检查不过：裸 LF / 裸 CR / NUL / 超过 998 字节的行、出口不允许的头、信封地址含 CR/LF/`<>`、超过 `SIZE`、需要 `SMTPUTF8` / `8BITMIME` / `AUTH PLAIN` 而服务器没有 | 全部 rejected，码为 0 | rejected |
| 提交点之前：连接、TLS、超时、断开 | 未定的全部 deferred，码为 0 | failed |
| 提交点之前：问候 / `EHLO` / `AUTH` / `MAIL` / `DATA` 回 4xx 或 5xx | 未定的全部按码：4xx deferred，5xx rejected | failed 或 rejected |
| `RCPT` 逐个回复 | 4xx deferred，5xx rejected，2xx 等最终回复决定 | — |
| 没有一个 `RCPT` 被接受 | 不发 `DATA`、不调 `commit` | failed（有 4xx）或 rejected |
| `commit` 返回错误 | 未定的全部 deferred，`Committed=false` | failed |
| 提交点之后：最终回复 2xx | `RCPT` 接受的全部 accepted | accepted；有被拒的收件人时 partial |
| 提交点之后：最终回复 4xx / 5xx | `RCPT` 接受的按码 deferred / rejected | failed / rejected |
| 提交点之后：没等到回复（断开、超时、取消）或回复不成形 | `RCPT` 接受的全部 unknown | unknown |

`failed` 与 `rejected` 都是「确定没送达」，区别是有没有值得重试的收件人；
`partial` 之后只重试 deferred 的那几个（§4.5）。`Send` 总是返回结果，
`accepted` / `partial` 时 error 为空，否则 error 说明原因（服务器回复包成 `*textproto.Error`）。
按码分类意味着 `AUTH` 回 `535` 是 rejected：重发同样的字节不会有不同结果，
但它其实是出口配置坏了而不是这封信有问题。所以分类不改，另给调用方一个判断：
会话开始之后的失败都是带阶段（`Stage`）的 `*transport.Error`，`ExitFailure` 对连接、TLS、问候、
`EHLO`、`AUTH` 这几个 `MAIL` 之前的阶段返回 true。outbox 据此暂停整个出口、信留在队列里，
不让一次凭据失效把队列逐封判死（§4.5「执行器」）。

**出口的配置**在进程环境里，按名字一组变量（`YUUKA_TRANSPORTS` 列名字，每个名字有
`_HOST`、`_PORT`、`_USERNAME`、`_PASSWORD_FILE`、`_CAPS`，清单和默认值见
[`cmd/yuuka/main.go`](../../cmd/yuuka/main.go) 的头注释，解析在 [`transports.go`](../../cmd/yuuka/transports.go)）。
密码只能放在文件里、变量里写文件路径：环境会出现在 `docker inspect`、`/proc/<pid>/environ` 和配置转储里，
文件可以单独设权限、单独挂载。文件只在启动时读一次，换密码要重启。
`_CAPS` 必填，可选预设 `cloudflare`（全部 false）、`standard`（`message-id,return-path`）、`none`，或显式列出；
不给默认值，是因为 caps 决定从每封信里剥掉哪些头，默认错了会悄悄改掉别人的信。

这是现状。**已定、正在实现的是把出口搬进库**（§17）：主机、端口、认证方式、用户名、caps 是 `transports` 的列，
密码加密存进 `secrets`，密文绑定到「主机 + 端口 + 用户名」，改其中任何一样都要重新输入密码（§17.2）；
每个身份一条出口链（§9.3），执行器每次领取时从库里解析（§10.2.2）。表和链已经建好
（[0017](../../internal/store/migrations/0017_transports.sql)），环境变量里的每个出口名在库里有一行占位；
执行器已经从库里解析（§17.9 的 P3），这组变量只在弃用期内顶替同名的占位行，库里配置好的那一行总是优先。上面这组变量届时只在启动时导入一次，
弃用一个版本后删除（§17.7）。不变的是：caps 仍然必填、没有默认值；密码仍然不经过环境变量和命令行参数；只有隐式 TLS。
envelope 的反向路径用 outbox 的 `from_addr`；VERP（§10.4.2）还没做，`CallerReturnPath` 目前不改变行为。

**校验而不修复**。`DotWriter` 会把裸 LF 悄悄改成 CRLF，于是出口收到的字节和 outbox 存下的 Sent 副本不一致；
裸 CR、超长行则在后面某一跳被拒或被改写。这些都是组装方的 bug，所以 transport 在连接之前检查、
不通过就 rejected，不替组装方修。8-bit 内容同理：头部有非 ASCII 要求服务器支持 `SMTPUTF8`（RFC 6532），
正文有非 ASCII 要求 `8BITMIME`，只在需要时带上对应参数。

**去掉出口不允许的头**由 [`StripHeaders`](../../internal/transport/message.go) 做：
`Return-Path` 总是去掉（它由最终投递方按信封加），`CallerMessageID=false` 去掉 `Message-ID`，
`CallerDKIM=false` 去掉 `DKIM-Signature`。它放在 transport 包里，因为规则是出口的；
放在组装器里不够，submission 口（§13.6）收进来的 raw MIME 不经过组装器。
调用方在写 Sent 副本**之前**调用它，所以存下的就是出口收到的字节；`Send` 自己只检查，
带着这些头的信直接 rejected，不悄悄删。

**连接只有隐式 TLS**（RFC 8314，默认 465），不支持 STARTTLS 和明文。证书按系统根校验；
配置里唯一的口子是换一组根证书（测试用自己的 CA），没有跳过校验的选项。
上下文没有截止时间时整次尝试最长 10 分钟，对应 RFC 5321 §4.5.3.2 里最长的那个等待（等最终回复）。

**回复原文要留着**。多数出口 `250` 的格式还没测过（§10.4.1），所以 `SendResult` 原样保存最终回复；
`ProviderID` 只从毫无歧义的两种形状里取：`queued as <id>`（Postfix），和整条回复恰好是
`2.0.0 Ok <msg-id>`（Cloudflare，取尖括号里的部分）。`MessageID` 只从用收到的副本验证过的形状里取，
目前只有 Cloudflare 这一种，而且只在出口自己生成 Message-ID（`CallerMessageID=false`）时才填；
其余一律留空，不去猜——猜错的 Message-ID 会让回信静默地拼不上线程。

**关键决策：MIME 由我们自己组装（[`internal/compose`](../../internal/compose/compose.go)），transport 只投递字节。**
好处是线程头（`In-Reply-To` / `References`）完全可控，Sent 副本就是我们提交的字节。
但它**不一定等于收件人收到的字节**：有的出口会加上或替换 `Message-ID`、`DKIM-Signature`、
信封发件人（Cloudflare 三样都动，正文和其余的头原样，§10.4.1）。这些差异用 `SendResult` 回填，而不是假装不存在。

由此得出一个让事情简单很多的结论：**优先用 provider 的 SMTP 口，而不是它的
结构化 REST API**。因为 SMTP 是唯一普遍接受 raw MIME 的接口。于是：

- `transport.SMTP` 一个实现覆盖：Cloudflare Email Sending、Resend、SES、
  Postmark、Mailgun、Gmail、工作邮箱、任何自建 MTA——差别只是
  `host/port/认证方式(PLAIN vs XOAUTH2)`，是配置不是代码
- 只有 **API-only** 或 API 明显更好的才单独写 driver（Gmail API 的 `messages.send`
  接受 base64url raw，配合它的增量拉取更顺；SES 的 `SendRawEmail` 便于拿 SNS 事件）

#### 10.2.1 Markdown → MIME

撰写用 Markdown（§12.1），所以组装规则要定死。实现在
[`internal/compose`](../../internal/compose/compose.go)：输入一封信的描述，输出交给 transport 的完整字节，
不碰数据库和网络。渲染器用 **goldmark**（Go 生态标准、CommonMark 合规、可扩展），
这是组装唯一的第三方依赖；头部编码、折行、quoted-printable / base64 都用标准库。

完整的 MIME 树（这是实现时最容易搞错的地方）：

```
multipart/mixed                         ← 只在有普通附件时出现
├── multipart/related                   ← 只在有内联图时出现
│   ├── multipart/alternative
│   │   ├── text/plain   ← Markdown 源码本身
│   │   └── text/html    ← goldmark 渲染 + 样式内联
│   └── image/*  (Content-ID)   ← 正文里引用的内联图
└── application/*  (attachment) ← 普通附件
```

空的层级不写：只有一个孩子的 multipart 不携带信息，有的客户端还会把它显示成一个空附件。
所以最简单的信就是一个 `multipart/alternative`。

几条硬规则：

- **`text/plain` 直接用 Markdown 源码**。这是选 Markdown 最大的收益：
  纯文本部分天然可读，不需要另写一个"HTML 转纯文本"的降级器（那玩意儿永远写不好）。
- **HTML 必须把样式内联**。邮件客户端普遍剥掉 `<head>` 和 `<style>`（Gmail 就会），
  所以不外链 CSS、不写 `<style>`、不出 `class`，表格补 `border`/`cellpadding` 属性给老客户端兜底。
  内联的做法是在渲染前给 AST 节点设 `style` 属性，由 goldmark 自己渲染出来，
  而不是渲染完再跑一遍 premailer 式的 inliner：后者要把自己的输出再解析一次，还要多一个依赖。
  代价是样式只能落在 goldmark 会渲染属性的节点上（引用、行内代码、图片、分割线、表格），
  代码块和原始 HTML 由自己的渲染函数接管。
- **原始 HTML 不透传，转义成文本显示**。草稿可能是读过不可信来信的 agent 写的（§8.3），
  `<script>`、`<img onerror>` 一类必须失效。goldmark 默认是丢弃并留一句注释，
  这里改成转义后照原样显示：收件人在 HTML 里看到的和 plain 部分一致，审核者看到的也就是发出去的。
  `javascript:` 链接沿用 goldmark 的过滤（`href` 置空）。
- **换行即换行**（hard wraps，同 GitHub 评论）。草稿在文本框里写，签名、地址都靠单个换行分行；
  按 CommonMark 把它们并成一段会让 HTML 和 plain 两份长得不一样。
- **签名按 RFC 3676 §4.3 的惯例**：身份的签名也是 Markdown，由组装接在正文后面，不先拼进正文：
  plain 部分是空一行、`-- ` 一行、签名；html 部分是一个灰色的块，分隔符是签名第一段的第一行（`--<br>`），
  不自成一段：自成一段时，邮件客户端和把 HTML 拆成段落的阅读视图（§8.1）都会显示一个孤零零的「--」。
  签名不以普通段落开头（列表、标题），或第一行会和分隔符合成一个 setext 标题时，分隔符才单独占一行。
  存的签名自己以 `-- `（或手打的 `--`）开头时去掉这一行，分隔符只写一次。
  拼进正文的做法会让 `-- ` 和签名第一行被 Markdown 读成一个标题，签名自带分隔符时还会出现两条。收信侧的摘要（snippet）在 `-- ` 一行处截止，列表里看到的是正文而不是签名
- **`>` 引用在两份里都天然正确**：plain 部分 `>` 就是邮件引用的原生语法，
  html 部分渲染成 `<blockquote>`。Markdown 和邮件在这一点上恰好同源。
- **代码块**不做语法高亮——邮件里 class 样式会被剥掉，`<pre>` 加内联背景色就够。
- **扩展只开 GFM 的表格、删除线、裸链接**。任务列表会渲染出表单控件，不开。
- **正式邮件的一个权衡**：plain 部分会裸露 `**bold**` 这类标记。
  Markdown 的 plain 可读性本来就是设计目标，日常无妨；但对外发正式邮件
  （如漏洞报告）时略显业余。所以给一个按身份配置的开关：
  plain 部分可选"去标记"渲染（`**bold**` → `bold`），默认保留原文以保真。
  去标记时链接写成「文字 <URL>」，代码块缩进四格，列表符号和 `>` 引用保留（它们本来就是纯文本的写法）；
  删除线保留 `~~`，因为去掉它会把意思说反。

组装上的几个决定：

- **输出是输入的纯函数**。Date 和 Message-ID 由调用方给，multipart 边界从它所分隔的内容的哈希派生，
  不取随机数。同一份草稿永远得到同样的字节，重试发出的就是提交时的那一份，测试直接比 golden file。
  边界以 `=_` 开头：quoted-printable 和 base64 的输出里不可能出现这个序列，所以边界与正文不会撞；
  哈希只负责区分嵌套的层级。哈希的输入是收件人本来就能看到的各部分，Bcc 不在其中，不泄露什么。
- **`Message-ID` 由调用方决定写不写**：`CallerMessageID=false` 的出口传空，组装就不写（§10.4.1）。
  生成器是 `<ULID@域名>`（§11.2）。不写 `X-Mailer`，任何头里都不出现项目名。
- **Bcc 不进头部**，只进 `Recipients()` 返回的信封收件人，由调用方交给 transport。Bcc 的显示名也不写到任何地方。
- **收件人的显示名照作者写的写进 To / Cc**，由 `net/mail` 编码：ASCII 名字遇到 RFC 5322 的特殊字符（`,` `.` `"` `<` `@` 等）
  加引号并转义，非 ASCII 名字写成 RFC 2047 encoded-word，长的按 encoded-word 折行。名字里的控制字符先换成空格，
  起不了新的头部字段（存储层本来就拒收带换行的名字，这里是第二道）。名字写成另一个地址（`"ceo@example.com" <x@example.net>`）
  在线上是合法的，挡不住也不该挡：它只是个名字，判断和审核卡片都看地址（§8.9）。golden 用例 `recipient-names`
  覆盖中文名、需要引号和转义的名字、伪装成地址或头部的名字，往返测试断言 mimeparse 读回的每个名字和地址都是写进去的那一个
- **回信的 `References`** = 父信的 `References`（没有就用它唯一的 `In-Reply-To`）加父信自己的 id
  （RFC 5322 §3.6.4）。父信来自外部，格式不对的 id 直接丢弃而不是写出去，重复的只留一个。
  链长超过 20 时保留第一个（线程根）和最近的 19 个，砍掉中间——线程算法只需要根和最近的祖先，
  RFC 5537 §3.4.4 也是这么修剪的。
- **正文编码取 quoted-printable 和 base64 中较短的一个**：以 ASCII 为主的信用 QP，
  Sent 副本里还能直接读；中文为主时 QP 会膨胀到三倍，改用 base64。主题和文件名的 RFC 2047
  encoded-word 按同样的规则在 Q 和 B 之间选（显示名交给 `net/mail`）。附件一律 base64，字节原样。
- **附件文件名写两处**：`Content-Disposition` 用 RFC 2231（标准写法），`Content-Type` 的 `name`
  用 RFC 2047（老客户端只认这个）。文件名限 255 字节，这样 2231 编码后的参数（无法折行）仍在 998 octets 以内。
- **地址只收 ASCII**。非 ASCII 的本地部分要 SMTPUTF8，还没有出口协商它；国际化域名请传 A-label。
  `multipart/*`、`message/*` 作附件暂不支持（RFC 2046 不允许它们用 base64），以附件形式转发等需要时再做。

#### 10.2.2 出口链与执行器

> 状态：已实现。解析、缓存、暂停在 [`internal/outbox/route.go`](../../internal/outbox/route.go)，
> 领取和停车的 SQL 是 [`store.ClaimOutbox`](../../internal/store/outbox.go)、`ParkUnusable`、`RouteAttempt`。

身份的出口链为什么按身份设、为什么只在出口故障后用备用，见 §9.3。这里写执行器的规则：

- **入队**：按链上所有**已配置且启用**的成员里最严的 caps 去头（取交集：任何一个成员不允许的头都去掉），写 Sent 副本
  （[`Service.Caps`](../../internal/outbox/route.go)）。上锁的出口算在内：caps 不是机密，主出口上锁正是链要兜住的情况；
  弃用期内环境变量顶替的占位也算（§17.7），没人配置的占位和停用的不算。
  链上一个算数的成员都没有时拒绝入队：剥哪些头定不下来，字节没法准备。
  `status-query` 不参与取交集：它不改字节，说的是能不能对账，跟着实际发出这封信的那个出口走（`send_attempts.transport`）
- **领取**：按位置取第一个**可用**、且 caps 允许这份字节的成员。可用 = 已配置、启用、机密解得开（§17.3）、没有被暂停。
  正常情况下就是主出口；主出口因出口故障被暂停时落到下一个，暂停期满后下一次尝试回到主出口。
  **决定：主出口是占位、被停用或上锁时同样用备用。** 运行时触发切换的只有暂停；其余三种是配置状态，不是这封信的结论，
  身份的链本来就是「主出口不能用时走这里」，链上的备用已经为这个域的 DNS 授权过（§9.3）。
  另一种做法是这三种情况停车等人，代价是主出口的一次配置事故（密码过期、KEK 没挂上）让本来能发的信全部积在队列里。
  主出口这次因出口故障被暂停、链上还有别的成员时，这次尝试记 `abandoned` 并立刻到期，下一次领取就走备用，不等暂停期满
- **「caps 允许这份字节」只能读了字节才知道**，而字节在领取之后才读（领取的事务拿着内容锁，不能等 R2）。
  所以领取先按可用性选出成员，读出 Sent 副本后再从链上挑第一个允许它的成员；挑中的不是领取时那个，
  就在提交点之前改写这次尝试的 `send_attempts.transport`（`RouteAttempt`，和提交点一样要求尝试仍属于这个执行者、租约未过期）
- **没有成员能发这份字节**（比如入队之后有人收紧了某个出口的 caps，或者把备用换成了更严的出口）：这次尝试记 `abandoned`，
  `outbox.error` 写明原因，10 分钟后再看，不计入重试上限。**永不判拒收**：字节没问题，是配置变了。
  原来的设计是和下一条一样不建尝试，但这要先读字节再领取；为一种只在改配置后才出现的情况，在每次领取前多读一次 R2，不值得。
  代价是这种信每 10 分钟多一条 `abandoned` 的尝试
- **链上没有可用成员**（占位、停用、上锁，或者都暂停了以外的情况）：不领取、不建尝试、不计数，
  定期的停车把每个成员不能用的原因写进 `outbox.error`，但**不推迟 `next_retry_at`**：配置一改好，下一次领取就发（§17.5）。
  全部成员只是被暂停的信不算停车，暂停期满自然会被领取
- **越过提交点之后不切换**：`unknown` 仍交给人（§4.5）。换个出口重发可能让对方收到两封，不比等人确认好。
  一次尝试只经一个出口；换出口只发生在下一次领取，而只有确定没送达的信才会被再次领取
- **暂停按出口 id 记**，不按身份：同一个出口坏了，用它的所有身份都该知道。出口或它的机密换了 revision 就清掉暂停（§17.5）
- 每次尝试在 `send_attempts.transport` 记实际用的出口名，出口名因此不可改（§17.4）

### 10.3 能力矩阵

| 出口 | raw MIME | 自定 Message-ID | 任意 From 前缀 | 附件/收件人上限 | bounce 回流 | 计费 |
|---|---|---|---|---|---|---|
| **CF Email Sending (SMTP 465)** | ✓ | ✗ 平台生成，自带的被替换；最终回复里给出实际的 id（§10.4.1） | ✓ 域级 onboard | 25 MiB / 50 | suppression list + API；不能自定 Return-Path | 3000/月含，$0.35/1k，需 Workers Paid |
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
   不是默认成立的事。Cloudflare Email Sending 把 `Message-ID` 列为平台控制的头，由它生成：
   API 下调用方带了这类头整封请求会被拒（`E_HEADER_NOT_ALLOWED`），SMTP 下不拒，但把它换掉（见下方实测）。所以：
   - `CallerMessageID=false` 的出口，组装时**不写** `Message-ID`，发出后把出口实际用的那个
     回填到 `send_attempts.message_id`。平台每次发送都重新生成，`partial` 之后的重试会拿到另一个，
     所以回填按尝试记、线程匹配认全部（`message_id` 和 `provider_id` 都认，§4.1）。否则对方回信的 `In-Reply-To` 对不上本地任何一条
   - 回填从哪里拿：Cloudflare 的 SMTP 最终回复就带着它（`SendResult.MessageID`），不需要事件订阅、
     Activity log 或 Worker send binding。别的出口接入时照样实测；SMTP 给不了时才轮到那几样，
     其中 binding 返回的 `messageId` 是否等于头仍未测
   - 都拿不到时，靠对方回信 `References` 里我们自己写进去的上游 id 拼线程；
     只有一条新线程的第一封会断开，落到 `class=human` 的 `subject + sender` 兜底匹配（§4.1）

   **Cloudflare SMTP 出口的实测（2026-10-31）**：从一个已 onboard 的测试域发给同域的 catch-all，
   收到的原件由 Email Worker 写进 raw bucket，与提交的字节逐字节比对。共 4 封：不带 Message-ID、
   自带 Message-ID、带 `In-Reply-To` / `References` 且主题、显示名、正文是中文并带附件的回信、
   自带一个（故意无效的）`DKIM-Signature`。

   | 项 | 结果 |
   |---|---|
   | `EHLO` | `AUTH PLAIN LOGIN`、`SIZE 26214400`（25 MiB）、`8BITMIME`、`ENHANCEDSTATUSCODES`；**没有** `SMTPUTF8`、`PIPELINING`、`CHUNKING`、`DSN` |
   | 最终回复 | 整条是 `2.0.0 Ok <36 位字母数字@From 的域>`，每次发送都不同；**4 封全部等于收到的 `Message-ID` 头** |
   | 不带 `Message-ID` | 平台加一条，就是回复里那个 |
   | 自带 `Message-ID` | 不拒收，但**被替换**：我们的 id 在收到的信里哪儿都找不到，只剩平台的一条 |
   | 自带 `DKIM-Signature` | 不拒收，**原样保留**，收件方照常验证（本例因为故意写错 `bh=` 判 neutral） |
   | 平台的 DKIM | 两条：`d=<From 的域>; s=cf-bounce`（与 From 对齐，DMARC 靠它）和 `d=cloudflare-smtp.net; s=cf2024-1`。签名的头列表含 `Feedback-ID`、`List-Unsubscribe` 等，**不含 `Message-ID`** |
   | 信封发件人 | 换成每封不同的 `<哈希>@cf-bounce.<From 的域>`；我们的 `MAIL FROM` 不出现在投递里 |
   | 加的头 | 在我们的头之上依次加 `DKIM-Signature` ×2、`Feedback-ID: <域>:5:5:Cloudflare`、`Received: from authenticated-submission.invalid by mx.cloudflare.net (Cloudflare Email Submission) with ESMTPSA id …`、`Message-ID`。**不加 `X-Mailer`**、`User-Agent`、`List-Unsubscribe` |
   | `Received` 链 | 提交那一跳不暴露我们的主机名和 IP；外发主机是 `*.cloudflare-smtp.net`，它的 HELO 没有 SPF 记录（`spf=none`，不影响 DMARC） |
   | 我们的头 | 全部原样保留，次序不变、连续，`Date` 不改，折行与 encoded-word 逐字节一致 |
   | 正文 | 除末尾多出的一个空行外逐字节一致（QP、base64、附件、multipart 边界）。那个空行是入站一侧加的，与出口无关（§10.1.1） |
   | 耗时 | 连接到最终回复 1–2 秒 |

   由此：
   - `transport` 在 `CallerMessageID=false` 时把回复里的 id 填进 `SendResult.MessageID`（同时也是 `ProviderID`），
     outbox 按尝试回填 `send_attempts.message_id`。CF 出口发出的信，对方回信的 `In-Reply-To` 直接对得上
   - `CallerMessageID` 仍是 false：我们的 id 会被丢掉，带了也没用。组装照旧不写，Sent 副本里也就没有一个
     「从未发出过」的 id；投递用的那个从回复里拿
   - `CallerDKIM` 仍是 false：自签的签名能原样到达，但平台已经用 From 域签了一条对齐的，自签不增加什么（§10.4.4）
   - 没有 `SMTPUTF8`：非 ASCII 的地址发不出去，头部必须是 encoded-word——组装本来就这么写

   **接入任何新出口的第一个测试：发一封给自己，比对 Sent 副本和收到的字节**，
   把差异逐项落进 `transport.Caps`。

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
   Cloudflare 把 `Return-Path` 列为平台控制、指向它自己的退信处理器（实测信封发件人是每封不同的
   `<哈希>@cf-bounce.<From 的域>`，§10.4.1），所以 **CF 出口没有 VERP**：
   只依赖它的 suppression list 和事件，该出口的 DSN 路径关闭，伪造 DSN 也就无从喂回状态机。

3. **能力不对等会漏到 UI 和 agent 面**。`SourceCaps` / `transport.Caps` 必须显式，
   UI 按 caps 灰掉做不到的操作（POP3 账户没有"标记已读回写"、
   `metadata` 策略的账户不能全文搜、Gmail 的 label 不是 folder），
   而不是假装都一样然后运行时报错。**抽象要诚实**。

4. **出口可替换的真正成本在 DNS，不在代码。**
   想要哪天能切到备用出口，SPF 的 `include` 和 DKIM selector 必须**提前**
   给所有候选出口配好。否则"替换"那天要等 DNS 生效，而且期间的信大概率进垃圾箱。
   建议现在就把 CF + 一家备用（SES 最便宜）同时授权，身份的出口链（§9.3、§10.2.2）
   支持 primary + fallback 才有意义。
   既然 MIME 由我们组装（§10.2），在 `CallerDKIM=true` 的出口上 **DKIM 可以在 Go 侧自签**：
   自有 selector、私钥在自己机器上，`d=` 对齐 From 域，DMARC 不依赖 provider 的签名。
   但 Cloudflare 把 `DKIM-Signature` 也列为平台控制：API 下自签的信整封被拒；
   SMTP 下收下并原样带出去，同时平台自己用 From 域（selector `cf-bounce`）再签一条（§10.4.1 实测）。
   平台那条已经对齐，自签不增加什么——**CF 出口只按它自己的签名算**。所以「出口可替换」的 DNS 成本是：
   CF 的 DKIM 记录 + 每个备用出口的 DKIM（或自签 selector）+ 各自的 SPF include，都要**提前**配好。
   入口侧对应的成本是 MX 切换：catch-all 的 MX 只能指一家，迁移期双收靠
   `content_hash` 折叠兜住（§4.1 的机制正好复用）。

### 10.5 一致性测试

driver 层做**契约测试**：一组 `.eml` 语料（多部分、中文编码、
超长 header、畸形 MIME、DSN、带 inline 图）+ 一套契约断言
（幂等、游标可续、raw 字节不变、envelope 正确提取）。
**任何新 driver 跑通这套就能接入**——这是 driver 可替换性唯一的真实保证，
不是接口声明。

---
