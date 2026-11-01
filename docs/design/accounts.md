# 多账户整合

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

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

**M3.5 先做的**（已定，未实现）：`kind=imap`，`sync_policy=full`，先接 Gmail（app password）；
同一套代码覆盖支持隐式 TLS 和密码登录的其他 IMAP 服务器。`metadata` 是 M3.5 的最后一步，可以单独砍出去：
它要 `FetchBody` 的远端定位验证、每次取正文记 `actions`、`no-store`、agent 取正文要人授权（§4.4），
是一整条新的读路径，`full` 跑稳之前不叠上去。`notify` 和它的 `notifications` 表在 M3.5 之后，目前没有用户。
需要 OAuth 的账户（M365、走 OAuth 的 Gmail）在 M3.5b（§17.8）。表上的形状见 §4 的设计块。

**回写默认关闭**，单向拉取（M3.x 里 `writeback_policy` 只允许 `off`，回写本身不做）。开启后只允许 `read / archive / label` 三种，
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
  实现见 [`store.ReplyIdentities`](../../internal/store/identities.go)：每条可见投递在它自己的账户里按精确地址、
  再按 `*@zone` 解析；我们自己发出的信取它发出时的身份。一封信投到了我们的好几个地址时（To 一个、Cc 另一个），
  候选按「提交者自己拥有的身份 → 精确身份先于 `*@zone` → 最早的投递 → 地址」排序：
  和运维者一起被抄送的 agent 用自己的身份回，运维者用自己的，而不是落到对方的地址上、再被硬上限压成草稿。
  草稿的 `identity_id` 总由 `from_addr` 解析得出，不由调用方指定，地址和身份不会对不上。
- **拉取投递的 zone / prefix 是这个邮箱自己的地址**（M3.5，已定）：`(account_id, zone, prefix)` 就落在这个拉取账户的身份上，
  上面的解析一行不改就成立，回 Gmail 的信从 Gmail 的身份、Gmail 的出口走。`account add` 顺带认领这个地址，
  否则 §4.3 的降级会降掉这个邮箱的每一封信（理由见 §10.1.4）。拉取投递没有信封：`envelope_from` 为空（NULL），
  `envelope_to` 是邮箱地址（§4）。Gmail 用别名收到的信，以后靠 `Delivered-To` 区分，M3.5 不做。
  Gmail 的 SMTP 出口今天就能配（`smtp.gmail.com:465`、`plain`、app password），只要一个属于这个拉取账户的身份；
  `identity add` 在库里有多个账户时要求 `--account`，所以加了第一个拉取账户之后，不带它的旧脚本会报错，这是预期的
- 谁能用哪个 identity 发信由 `identity_grants` 决定（§13.5），
  服务主体无法冒用运维者的地址。
- outbox 每条带 `identity_id`，路由由 identity 决定，不给调用方选 transport 的机会。
- 出口和链在库里（[0017](../../internal/store/migrations/0017_transports.sql)）：
  每个 identity 一条出口链，升级时原来的出口成了链上唯一的成员；库里的出口在配置之前是只有名字的占位行，
  弃用期内由环境变量里同名的出口顶替（§17.7）。执行器按链发信（§10.2.2）。
- **出口进库，每个 identity 一条出口链**（§17；`identity_transports`：位置 0 是主出口，后面是备用，最多 8 个，
  连续性和上限在 [`store.SetIdentityTransports`](../../internal/store/transports.go) 里查）。
  - **链按身份设**，不做命名的链，也不做出口级的「这个出口的备用是那个」。备用合不合法取决于这个身份所在域的 DNS：
    SPF 的 `include` 和 DKIM 要提前给备用出口配好（§10.4.4），一个服务十个域的出口，可能只有其中几个域授权了备用。
    挂在出口上的备用会把没授权的域也一起带过去，信在收件方那里判为伪造
  - **只在主出口不可用时才用备用**。运行时能让它不可用的只有出口故障：`MAIL` 之前的失败（连不上、`AUTH` 被拒，§10.2 的 `ExitFailure`），
    也就是执行器会暂停这个出口的那一类；其余是配置状态（占位、被停用、机密上锁），同样走备用，是一项明确的决定（§10.2.2）。
    收件方的 4xx / 5xx 是这封信的结论，换出口不会有不同答案，照常退避重试同一个出口。
    越过提交点之后不切换，`unknown` 仍交给人（§4.5）：换个出口重发可能让对方收到两封
  - **入队时按链上最严的 caps 去头**：Sent 副本在入队时就定了（§4.6），之后每次尝试发同样的字节，
    所以这份字节要能从链上任何一个成员发出去。代价是主出口能自定 Message-ID、备用不能时，经主出口发出的信也没有自己的 Message-ID，
    线程靠回填（§10.4.1）
  - 执行器怎么在链上挑出口、链在入队之后变了怎么办，见 §10.2.2
- identity 是精确地址或 `*@zone`。精确地址压过同 zone 的通配：给服务的授权应当落在精确地址上，
  `*@zone` 上的授权碰不到别人单独拥有的地址（表见 [0005](../../internal/store/migrations/0005_sending.sql)）。
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

M3.5 落下的 agent 面（已定，未实现）：

- **agent 永远碰不到来源的配置和凭据**：没有任何 MCP 工具读写 `account_sources`；REST 的账户管理端点只认人的会话（§13.10），
  `TestACL` 断言持有全部 scope 的 token 拿到 403
- 新增的读路径都过 §13.8 的可见性，进 `TestVisibility` 的夹具：
  - 账户列表（MCP `list_accounts`、REST `GET /api/v1/accounts`）只列这个主体可读的账户，只给名字、种类、地址，
    不给主机、用户名、同步状态
  - 搜索加 `account:<名字>`（§16.3），默认仍跨账户
  - agent 视图里的每条投递带账户名
- 回填的信不推送、不评分（§10.1.4）；拉取账户评不评分由主机决定（`YUUKA_TRIAGE_ACCOUNTS`）

### 9.5 落地顺序

放在 **M3.5**（§6）：等解析、存储、规则引擎稳定后再接外部账户。
原因是 IMAP 拉取的脏活（UIDVALIDITY 变化、Gmail All Mail 重复、
OAuth 续期、限速）会掩盖核心链路的问题，不适合早做。
数据模型的**形状**从一开始就是多账户的：内容 / 投递两层拆分、`deliveries` 带账户维度、
幂等键是 `(账户, source_key)`，catch-all 只是其中一组账户。
但 `accounts` / `identities` 两张表在 **M2** 建（[0005](../../internal/store/migrations/0005_sending.sql)）：
M0、M1 只有一个 Cloudflare 账户，用进程配置的 `account_key` 代替；
M2 发信时要按 identity 选出口、按 `identity_grants` 放行，这两张表才有了真正的读者。
0005 把每个 `account_key` 变成一个 `cf_catchall` 账户，`deliveries` 改用外键，幂等键改为 `(account_id, source_key)`。
`YUUKA_ACCOUNT` 从此是这个账户的名字，ingest 第一次用到时建行（§4）。
`accounts` 只建了 M2 读的列，kind 只有 `cf_catchall`，M2 的所有 identity 都属于这个账户。

§17 的出口和机密已经先于 M3.5 落地：拉取账户的密码要有地方存。

#### M3.5 已定的形状（未实现）

拉取账户的列和表见 §4 的设计块，这里写它们为什么是这样：

- **来源放在 `account_sources`**（IMAP 主机、端口、用户名、认证方式、机密引用、可信接收方、文件夹、大小上限、回填、预算、轮询间隔），
  一个账户一个来源。密码进 §17 的加密 `secrets`（类型 `imap_password`，audience 是 `imaps://<用户名>@<主机>:<端口>`），
  而不是 `credentials`（§13.3）或进程配置：一个人可能有好几个外部邮箱，来源要能在运行时增删，凭据的去向和凭据绑在一起加密（§17.2）。
  换主机、端口、认证方式或用户名必须带新密码、新 id，和出口一样在存储层查（§17.4）
- **Gmail 的 app password 存两份**：IMAP 来源一份（`imaps://…`），SMTP 出口一份（`smtps://…`），CLI 可以从同一个文件读两次。
  不为它破例做「一条机密多个 audience」：那会让「这个密码可以交给谁」不再是一个值（§17.8）
- **账户默认谁能读**：`accounts.default_read` 加 `account_access` 的行覆盖它（§13.8）。Cloudflare 账户默认可读（升级前后不变），
  拉取账户默认不可读，加账户的人自动得一行
- **删拉取账户 = 停用来源、抹掉密码**；账户、投递、原件全部保留，名字不再复用。和 `transport rm`、`identity rm` 同一个语义
- **同步**：每轮怎么拉、怎么提交、失败和限速怎么处理在 §10.1.3；回填和推送、评分的关系在 §10.1.4
- 一起建、暂不写的列：`deliveries.remote_state` 只写 `present`，`offloaded_at`、`accounts.offload_policy` 等 M3.6（§9.6）

**命令**（名字是对外标识，已定，不再改）：

```
yuuka account add <名字> --imap ...   # 加拉取账户；顺带认领邮箱地址（§9.3）
yuuka account set <名字> ...          # 改来源；换去向必须重新给密码
yuuka account ls                      # 账户、来源、同步进度（追新、回填、失败计数）、机密状态
yuuka account test <名字>             # 连接、TLS、登录、LIST、每个要同步的文件夹 STATUS；不取信
yuuka account sync <名字>             # 请 serve 立刻跑一轮；CLI 自己不拉（§10.1.3）
yuuka account pause <名字> | resume <名字>
yuuka account rm <名字>               # 停用来源、抹掉密码，其余保留
yuuka account access <名字> ...       # 谁能读这个账户：给一个主体可读、不可读，或回到默认；不带参数时列出
```

参数和用法随代码写进运维文档，这里只定名字。几处取舍：

- `rm` 不叫 `disable`，和 `transport rm`、`identity rm`、`channel rm` 一致
- 叫 `access` 而不复用 `grant`：`grant` 已经是发信授权（§13.5），混在一起，读命令的人会以为给了发信权
- `sync` 不在 CLI 进程里拉信，理由见 §10.1.3
- 密码只从文件或标准输入读，不接受参数值（§17.6）

**推迟的，和理由**：

| 推迟的 | 到哪 | 为什么 |
|---|---|---|
| `metadata` 策略 | M3.5 的最后一步，可单独砍 | 见 §9.2 |
| OAuth（Gmail OAuth、M365）、`oauth_clients`、出口的 `xoauth2` 和 STARTTLS | M3.5b | Gmail 个人账户用 app password 就够；M365 据说已关基础认证、只能 OAuth，它的 SMTP 提交口据说只有 587 STARTTLS（都待实测，§17.8），而出口现在只做隐式 TLS、还要新的 audience。三件事叠在一起，单拎出来做（§17.8） |
| IDLE | M3.5 之后 | 先轮询，理由见 §10.1.3 |
| 回写（已读、归档、标签） | 不在 M3.x | §9.2 默认关，`writeback_policy` 只允许 `off` |
| 远端消失检测、`remote_state = gone` | M3.6 | 要每轮列全部 UID、多轮确认（§10.1.2）。唯一的硬需求是卸载；M3.5 不删远端，不需要知道远端删了什么 |
| 归档卸载 | M3.6 | §9.6 |
| `smtp_only` 账户 | 有用户时 | `accounts.kind` 是封闭列表，没有读者的值不进 CHECK |
| Gmail API driver | 以后 | 要 OAuth 受限 scope；`source_key` 已经按它对齐（§10.1） |

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
