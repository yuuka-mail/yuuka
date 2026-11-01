# 多主体：一个邮箱宇宙，多个主体

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 13. 主体、凭据与权限

### 13.1 多主体不是多用户

需求是：运维者本人、一个具名 agent、若干机器主体（监控、定时任务、其他自托管服务）
都要能用这个系统，各有各的凭据和权限，但**不要企业邮箱那套组织架构**。

这两件事必须区分清楚，否则复杂度会失控：

| | 多租户（企业邮箱） | **多主体（本设计）** |
|---|---|---|
| 数据 | 每个用户/组织一份，互相隔离 | **同一份，所有主体看同一个消息宇宙** |
| 收件箱 | per-user | 没有 per-user 收件箱 |
| 权限 | 角色 + 组 + OU + 委托 | **扁平 scope，直接挂在凭据上** |
| 配额 | per-user | 不做 |
| 复杂度来源 | 隔离与授权矩阵 | 凭据生命周期 |

所以这一层叫 **principal（主体）而不是 user**。命名本身就是设计约束：
看到 `principals` 就不会顺手去加 per-user 文件夹，而 `users` 会。

**明确的非目标**：组织架构、群组、OU、共享邮箱、委托（delegation）、
per-user 配额、角色（role）层。权限只用扁平 scope 表达，不要 role → scope 的间接层。

### 13.2 principals

`actions.actor`（§8.8）此前是字符串，现在升为一等实体，`actor` 变成外键。
表定义见 [`0003_identity.sql`](../../internal/store/migrations/0003_identity.sql)：`kind` 是 human / agent / service / rule / system 五种之一；
`owner_id` 让服务凭据归属于某个 human，构成可撤销链。

实际会有的主体：

| 主体 | kind | 用途 |
|---|---|---|
| `human:<operator>` | human | 浏览器会话，全部权限 |
| `agent:arisu` | agent | 读、标注、起草；以自己的专属身份按授权发信，以人的身份只到草稿（§8.9） |
| `service:monitoring` | service | 只能用 `alerts@example.com` 发信 |
| `service:scheduler` | service | 只能用 `cron@example.com` 发信 |
| `rule:<id>` | rule | 规则引擎自动动作的归属（§8.5） |
| `system` | system | 系统事件；署名为 yuuka（§11.1） |

`display_name` 就是 §11.1 署名分层的数据来源——UI 和 TG 上"谁在说话"直接读这个字段，
不靠前端硬编码。

### 13.3 credentials：和主体分离

一个主体可以有多把凭据，各自独立撤销。`kind` 分 session / token / smtp_password / oauth 四种，
表定义见 [`0003_identity.sql`](../../internal/store/migrations/0003_identity.sql)。

分离的好处很具体：给某台机器发一把只能发信的 SMTP 密码，换机器时撤销这一把，
不影响同一主体的其他凭据，也不用改 principal 本身。
`last_used_at` 让"哪把凭据其实早就没用了"看得见。

`secret_hash` 只适用于 `session / token / smtp_password`——这三类只需校验。

**`oauth` 类和 `secret_enc` 列保持不用**（已定，未实现，§17.4）。原来的设计让 M365 的 refresh token 作为 `oauth` 类凭据、
加密存进 `secret_enc`。但这张表放的是别人用来向 yuuka 证明身份的凭据；外部账户的 token 是 yuuka 拿去向第三方证明身份的，
属于那个账户和它的来源，不属于任何主体。放在一起，「吊销一个主体的凭据」和「断开一个外部账户」就纠缠在一起。
所以 yuuka 的一切出站凭据（出口密码、bot token、IMAP 密码、OAuth token）都放进 §17 的 `secrets` 表；
`kind = 'oauth'` 和 `secret_enc` 不写也不删，删 CHECK 的枚举值要迁移，留着没有代价。

那张表的加密格式也一并定了（§17.2），是**信封加密**，为的是以后能换成 Vault Transit 或云 KMS 而不迁移数据：

- 每条机密一把自己的随机数据钥匙（DEK），载荷在本地用 AES-256-GCM 封住；载荷开头有格式版本，换算法时按版本区分新旧，不用猜
- DEK 由一个可替换的包装器包起来，库里存「包装器 id + 钥匙引用 + 包好的 DEK」。今天只有 `local`：
  钥匙是主机上的 KEK 文件，钥匙引用是它的指纹。以后的 Vault Transit（加密、解密、重新包装，钥匙带版本，带 context）
  或云 KMS（带 encryption context）接进来，只需要把 DEK 重新包装一遍，载荷不动
- 轮换和换包装器都只重新包装 DEK；Vault / KMS 只见过 DEK，从没见过机密
- 包装器不可用（没有 KEK 文件、Vault 连不上）时机密上锁，和缺 KEK 一样，收信不受影响（§17.3）

代价是每条机密多存一个包好的 DEK、每次解密多一次解包。现在就这样做而不是等真要接 Vault 时再改，
是因为单层格式以后换成信封，要在旧 KEK 和新系统同时可用的情况下把每一行重新加密一遍。
`pg_dump` 因此含封好的载荷和包好的 DEK，备份按敏感数据保管，KEK 单独离线备份（§7、§17.3）。

载荷要和它的用途绑定：AEAD 的附加数据（AAD）写进「固定的用途串、格式版本、机密 id、类型、audience」。
**audience 的绑定留在本地，从不交给 Vault / KMS**：检查要发生在明文被使用的地方，而且不该随包装器的上下文语义而变。
audience 是这份明文可以交给谁（出口的 `smtps://用户@主机:端口`、bot 的 `telegram:bot<id>`……），用的时候从拥有它的那一行重算，
对不上就失败即关闭。原来设想的「表、行 id、账户」挡得住把一行的密文复制到另一行，挡不住更直接的一种：
能改库的人不动密文，只把那一行的主机改成自己的，yuuka 就会把明文交过去。audience 两种都挡（§17.2）。
AAD 各字段**带长度前缀**拼接，不用分隔符：用分隔符拼接的话，字段里出现分隔符时两组不同的字段会拼出同一串字节。
gofer 第一版用 NUL 分隔，后来出了带长度前缀的第二版，再在启动时重写所有行（§1.2）。

可校验的三类秘密都由服务端生成，从不接受人选的值，所以 `secret_hash` 用 SHA-256 而不是慢哈希：
256 位的随机串没有可穷举的空间，慢哈希只会拖慢每个请求。交给持有者的字符串是
`<前缀>_<凭据 id>_<秘密>`，每类凭据一个前缀（格式见 [`internal/auth/secret.go`](../../internal/auth/secret.go)）：
明文里带 id，按主键取行再做常量时间比较，而不是按哈希搜索；前缀让泄漏的串能被认出来，
也让一类凭据不会被当成另一类接受。

passkey 不是 `credentials` 的一行：它是产生 `session` 凭据的登录方式，本身不带 scope，
私钥也从不离开验证器。它单独一张 `passkeys` 表，列按 WebAuthn 的 credential record 展开
（公钥、签名计数、AAGUID、BE/BS 标志），不把任何库的序列化格式冻结进库里。

**`token` 的签发（M1）**：主机上的 CLI（`yuuka token issue` / `ls` / `revoke`，
参数见 [`cmd/yuuka/token.go`](../../cmd/yuuka/token.go)），或 WebUI 设置页里先再验一次 passkey 的人（§13.10）；
两者走同一个 `authn.IssueToken`，下面的规则对两边都成立。

- 只发给 `agent` 和 `service`。`human` 的凭据只有 passkey 登录产生的 `session`，
  `issue` 遇到 human 直接拒绝并指向 `admin enroll`：给人发一把长期 token，等于在 passkey 旁边开一扇不防钓鱼的后门。
  这条在 `authn.IssueToken` 里也查一遍，不只靠 CLI
- 指名的主体不存在就建（`display_name` 先等于 name），建主体本身记一条 `principal.create`
- token 明文只给一次：CLI 单独一行写到 stdout（说明写 stderr），便于直接重定向进密钥文件；
  界面只在签发的回答里显示一次。库里只有哈希，丢了只能吊销重发。
  `--ttl` 缺省为不过期：token 靠显式吊销收回，`ls` 的 `last_used_at` 让闲置的那把看得见
- `ls` 连已吊销的也列出，永远不显示秘密
- `revoke` 只收 `token` 和 `smtp_password` 类凭据（会话由登出撤销），记 `auth.credential.revoke`。
  吊销立即生效：REST 和 MCP 的 HTTP 形态每个请求重新认证，stdio 形态每次工具调用重新认证（§8.7），
  SMTP 每个会话重新认证，没有任何缓存的认证结果要等它过期

**SMTP 提交口的密码（M2）**：`yuuka token issue --kind smtp --principal service:<name>`，同一套签发、`ls`、`revoke`。

- 交给持有者的密码是 `ykp_<凭据 id>_<秘密>`，**SMTP 用户名就是凭据 id**（命令单独打印出来）。
  提交口解析密码、要求其中的 id 等于用户名，再按 id 取行做常量时间比较
  （[`authn.Bearer.AuthenticateSMTP`](../../internal/authn/bearer.go)），和 token 完全一样；用户名和密码对不上一律拒绝，
  一把密码不能挂在另一把凭据的名下用。另一种做法是用户名写主体名（`service:monitoring`），
  但那样查找要么按哈希搜索，要么一个主体只能有一把密码；凭据 id 两个问题都没有。
  密码带前缀，泄漏时能被认出来，也不会被当成 bearer token 接受（反之亦然）
- scope **恰好是** `submission:send`：这类凭据只能做这一件事，scope 由类型决定，不是替人补的默认值；
  `--scope` 写了别的就拒绝。用哪个身份、发到哪一步照旧由 `yuuka grant` 决定，不由密码决定
- 和 token 一样只发给 `agent` 和 `service`

### 13.4 scope 是唯一的权限语言

scope 是一张封闭的扁平列表，定义在 [`internal/auth/auth.go`](../../internal/auth/auth.go)，
库里 `credentials` 上的约束是同一份（[`0003_identity.sql`](../../internal/store/migrations/0003_identity.sql)）：加一个 scope 要写迁移，这正是目的。

每把凭据持有一个 scope 子集。§8.2 的能力分级在这里落地：

| 主体 | 典型 scope |
|---|---|
| human 浏览器会话 | 全部 |
| `agent:arisu` 的 MCP token | `message:read` `delivery:read` `prefix:read` `message:annotate` `draft:create` `submission:send` |
| `service:monitoring` 的 SMTP 密码 | `submission:send` |

签发时 scope **必须逐个写明**：没有默认集合、没有 `all` 之类的简写，不认识的名字直接拒绝，
空列表也拒绝而不是当成「全部」或「只读」。多给一个 scope 的代价是一次越权，少给一个只是 agent 报一次「缺 scope」。

scope 只回答「能不能做这类动作」，**用哪个身份、做到哪一步由 §13.5 的授权表回答**。
所以 agent 持有 `submission:send` 不等于能发信：没有 `identity_grants` 的身份它用不了，
以人的身份它最多到草稿。两层都是 API 层的事实，不是提示词的约定（§8.2）。
注意 `message:annotate` 的动作必须可撤销（§8.8 的 `undo_token`）；agent 笔记的撤销就是删除（§8.10）。
`rule:manage` 是人的能力：规则是建好之后不再经人复核的常设能力，所以管理规则、以及接受提案时顺手建规则，
端点在 scope 之外还按主体类型只放人（§8.5）。agent 的 token 不该带它，带了也用不上。

### 13.5 identity ↔ principal 授权

§9.3 已经把 identity 硬绑到 transport。现在再加一层：谁可以用这个 identity 发信。

`identity_grants` 每行是一个（身份, 主体）对：档位 `send_policy`（§8.9）、收件人范围、各项限额、
`review` 档的撤回窗口和熔断时间。表见 [`0003_identity.sql`](../../internal/store/migrations/0003_identity.sql)，
列的默认值取安全的一端（`none` + `thread`）。由 `yuuka grant set|ls|rm` 管理（参数见
[`cmd/yuuka/grant.go`](../../cmd/yuuka/grant.go)）：`set` 只改给出的参数，其余保持原值，新行从列默认值起步；
`rm` 之后适用的是下面的默认，不一定是「什么都不能做」，命令会把它打印出来。

执行全在一个纯函数里：[`sendpolicy.Decide`](../../internal/sendpolicy/sendpolicy.go)，没有时钟、不碰数据库，
输入由 store 的 [`PolicyInput`](../../internal/store/policyfacts.go) 一次备齐。所有提交路径（REST / MCP 的
`submit_draft`、WebUI 的发送、SMTP 提交口）都经 [`internal/submit`](../../internal/submit/submit.go) 进入
[`store.WithDecision`](../../internal/store/policytx.go)：在持有授权锁的事务里读事实、调 `Decide`，再经同一个事务写 outbox 行或草稿，
并发提交不会一起越过最后一个名额。没有第二条路径，测试辅助和 CLI 也不例外——这由结构保证，见 §8.9。

**各项限额的默认值由授权代码决定，不写在库里**：库里的空值只表示「这条授权没有自己的数」，
值见 [`sendpolicy.DefaultLimits`](../../internal/sendpolicy/sendpolicy.go)，改默认值不需要迁移。按提交主体的类型：

| 主体 | 每小时 | 每天 | 每天新收件人 |
|---|---|---|---|
| human | 不限 | 不限 | 不限 |
| service | 30 | 200 | 5 |
| agent 及其他 | 10 | 50 | 5 |

取舍：数字故意低。限额触发的代价是信变成草稿、人看一眼；限额太高的代价是失控的 agent 或泄露的凭据
在没人注意时发出一整天的信。人按发送本身就是确认，所以人不设默认限额。需要更多就在授权里写明数字。
窗口是滚动的（最近 60 分钟、最近 24 小时），不依赖时区；按 outbox 行计数，不论状态，提交过就占一个名额。
「新收件人」是此前没有任何处于已发出或在途状态（`queued`…`failed`）的 outbox 行发往过的地址，按 `addr_fold` 比较。
收件人范围和限额都只看地址，收件人的显示名不参与任何一项（§8.9）。

`review` 档的撤回窗口 `hold_seconds` 同理：空值是 [`sendpolicy.DefaultHoldSeconds`](../../internal/sendpolicy/sendpolicy.go)（600 秒，
对所有主体一样），0 是明确不设窗口，其余是秒数；一个存着的值代表多长由 `sendpolicy.EffectiveHoldSeconds` 回答，
比较授权和实际生效值的地方都用它。10 分钟够在手机上读完卡片按撤回，又不至于让一封回信读起来像隔天的。

**硬上限在两处成立**：`Decide` 里身份属于某个 human、提交者不是他时一律压回 `draft`，不看授权表；
`yuuka grant set`（[`store.SetGrant`](../../internal/store/grants.go)）拒绝写入这种组合下高于 `draft` 的档位，
而不是存下来再在执行时压低——表里写着一个永远不会生效的 `auto`，只会误导下一个读它的人。
后者只是提示，前者才是保证：即使表被手改，`Decide` 照样压回去。

于是 `service:monitoring` 只能以 `alerts@example.com` 发，**无法冒用运维者本人的地址**。
服务凭据泄露的后果因此被限制在一个前缀内——这对 catch-all 尤其要紧，
因为域级 onboard 意味着"能发信"默认等于"能以任意前缀发信"（§9.3）。

档位和限额对所有主体是同一套机制，区别只在默认值。没有授权行时（[`sendpolicy.Effective`](../../internal/sendpolicy/sendpolicy.go)）：

| 主体 → 身份 | 没有授权行时的档位 |
|---|---|
| `human:<operator>` → 自己的身份 | `auto` + `any`（人按发送就是确认） |
| `agent:arisu` → `arisu@example.com`（owner 是它自己） | `review` + `thread`：审核通过后扣留 10 分钟可撤回，升级的停在草稿（§8.9） |
| service → 自己拥有的身份 | 同上，`review` + `thread` |
| `agent:arisu` → 人的身份 | `draft`，**且不能调高**（§8.9 的硬上限）：替人起草正是 agent 的用处（§8.2） |
| 其他组合（service → 人的身份、human → 别人的身份……） | `none` |

人对自己身份的默认是 `auto` 而不是 `none`：要求运维者先给自己写一条授权才能回信，只增加步骤，不增加安全——
能写授权的就是他本人。service → 人的身份默认 `none` 而不是 `draft`，因为 SMTP 提交口对 `none` 直接拒收（§13.6），
`service:monitoring` 写 `From: 运维者` 得到的是拒绝，而不是一堆草稿。
`service:monitoring` → `alerts@example.com` 的 `auto` + `allowlist` 是要显式写的授权；`alerts@` 的 owner 若是运维者，
硬上限会让它停在草稿，所以给服务专用的地址应当 owner 就是这个服务（或授权写在 service 自己拥有的身份上）。

服务主体也可以设成 `review` 或 `draft`：SMTP 客户端照常拿到 `250`，
信停在 `held` 或草稿里等审核 / 等人。这让「新接入的服务先观察一周」不需要另做机制。

### 13.6 SMTP submission：让其他服务直接发信

这是多主体带来的最大实用价值。yuuka 自己监听一个 **SMTP submission 口**
（`go-smtp` 的 server 侧，§5），
用 SASL 校验 `credentials`，然后：

1. **按信头 `From` 确定 identity**，不按 `MAIL FROM`。收件人看到的、DMARC 对齐校验的都是信头 `From`；
   只查信封，`service:monitoring` 就能用 `alerts@` 过校验、再在信头里写运维者本人的地址。规则：
   - 信头 `From` 必须恰好一个地址，且该主体对它有授权；多地址 `From` 直接拒收
   - 有 `Sender` 头时它也必须是该主体有授权的身份
   - `MAIL FROM` 必须等于信头 `From`；真正投递时信封发件人由 transport 重写（§10.4.2）
   - 显示名以 identity 配置为准，覆盖客户端写的——`"运维者" <alerts@example.com>` 这种显示名冒用同样要堵
   然后按该授权的 `send_policy` 处理（§13.5）；`none` 直接拒收
2. 规范化 MIME（按出口 caps 去掉不允许的头，§10.2），写 Sent 副本
3. 入 outbox 状态机，由 §10.2 的 transport 真正投递

落到代码上是 [`submit.SubmitRaw`](../../internal/submit/submit.go)（规范化在 [`raw.go`](../../internal/submit/raw.go)）：
信头 `From` 经 [`store.ResolveIdentity`](../../internal/store/identities.go) 找到 identity（找不到就拒收，`ErrNoIdentity`），
`Sender` 同样解析，且提交者对它的档位不能是 `none`；然后在 `store.WithDecision` 里决定：
档位 `none` 拒收（`ErrRejected`，550），否则一律 `250`——`Queue` 入 outbox，否则规范化后的字节写进 `sent/`、
以 `drafts.raw_id` 停在草稿，`hold_reason` 记原因。每次拒收也记一条 `submission.raw` 审计，只有地址和原因，不含正文。
`In-Reply-To` 指向一封提交者看得见的信时，按回信来决定（`thread` 范围、邮件循环规则都要用到它）；找不到就当新线程。

规范化定下的几条：

- 换行一律改成 CRLF；裸 CR、NUL、超过 998 字节的行拒收而不是修——修了就不是客户端交来的那封
- `From`（和 `Sender`）整行重写成 identity 的显示名 + 决定所用的地址；`Bcc` 头删掉，收件人只看信封，
  按信头 `To` / `Cc` 标记 `outbox_recipients.kind`，其余算 Bcc。To / Cc 的显示名原样保留；
  提交停在草稿时，草稿的收件人带上信头给的名字，只为显示（不合格的名字留空，发出的仍是原件），和撰写的草稿一致（§8.9）
- 缺 `Date` 就补（RFC 6409 §8.2）；缺 `Message-ID` 且出口投递我们的 id 时才补（§8.3），
  否则 `transport.StripHeaders` 照常剥掉客户端写的
- 决定要求 `Auto-Submitted` 时补上 `auto-generated`；客户端自己写了别的值（`auto-replied` 等）就保留，写 `no` 则替换
- 规范化是幂等的。停在草稿的提交被人确认时，按新决定再规范化一遍：字节没变就直接用存下的那份原件
  （`outbox.raw_id = drafts.raw_id`），变了（比如服务自己重提、这次要加 `Auto-Submitted`）就另存一份新的 Sent 副本。
  代价：停了很久的信，`Date` 是提交时补的，不是确认时的

**协议层**在 [`internal/smtpd`](../../internal/smtpd/smtpd.go)，只管协议；上面那些关于信的规则全在提交核心里，
协议层一条也不重复。它和核心之间是一个在消费方定义的小接口（`smtpd.Submitter`），`serve` 用
[`submitPort`](../../cmd/yuuka/submission.go) 把 `submit.Service` 接上去：核心的 `ErrRejected` / `ErrNoIdentity`
转成 `smtpd.ErrRefused`（550），其余错误都是暂时的（451）。实现时定下的几处：

- **只有 AUTH PLAIN**。LOGIN 从没走出草案，`go-sasl` 也不再带它的 server 侧；会说 SMTP 的服务端软件都会 PLAIN。
  真遇到只会 LOGIN 的客户端再加。用户名是凭据 id（§13.3），授权身份（authzid）只能为空或等于用户名，不做代理授权
- **AUTH 之前不收 `MAIL`**（`530 5.7.0`）。`submission:send` 在提交口再查一遍，不只靠签发时「SMTP 密码恰好这一个 scope」的约束
- **吊销对已经开着的会话也立即生效**：每个 `MAIL` 重新认证一次，为此会话结束前密码留在内存里。
  失效时回 `421`：客户端挂断、信留在它手里，重连时在 AUTH 得到 `535`——凭据坏了就该在那里暴露，而不是让信被逐封退回
- **上限取 Cloudflare 出口的实测值**（§10.3、§10.4 的实测表）：`SIZE` 25 MiB，每个事务 50 个收件人
  （第 51 个回 `452 4.5.3`，客户端把剩下的放进下一个事务）。比出口宽只会把拒绝推迟到回过 `250` 之后。
  规范化补的头可能让一封贴着上限的信超过出口的 `SIZE`，那会在 transport 的本地检查里成为 rejected，不会无声消失
- **不宣告 `SMTPUTF8`**：出口没有，需要它的信发不出去；非 ASCII 的信封地址回 `553 5.6.7`（RFC 6531）。
  `8BITMIME` 和 `CHUNKING` 是 `go-smtp` 固定宣告的；出口收 8-bit 正文，transport 发送前按出口能力检查
- **回复只由核心的结论决定**。停在 review 或草稿也是 `250`：留住它是 §13.5 的策略在起作用，
  回错误的话客户端会重试，草稿越堆越多。回复文本带上 id，运维者据此找到那封信

  核心以外的错误（库、R2……）一律 `451`，原因只进日志：客户端留着信重试，宁可重复也不丢。
  超过 `SIZE` 和收件人上限的在协议层就挡掉，到不了核心。每种结论对应的回复码见[接入 SMTP](../integrate/smtp.md#回复码)

- **防滥用**：连接数有总上限和每个客户端的上限（IPv4 按地址，IPv6 按 /64：一台主机通常拿到一整个 /64，
  按地址算它每次都能换个新桶），超了回 `421` 断开；AUTH 失败每个客户端先放几次、之后按固定间隔放一次，
  超了不查库直接 `454`。密码是 256 位随机串，限速保护的不是密码，而是一个客户端能引起的查库次数和审计条目数。
  每条命令最多等 5 分钟（RFC 5321 的下限；`go-smtp` 按命令行设读超时，所以它也是整个 `DATA` 的上限），
  一行最长 2000 字节。数字按「同一部署环境里几台服务」定，见 [`limit.go`](../../internal/smtpd/limit.go)。
  连接计数挂在原始 TCP 连接上：`go-smtp` 只对 `*tls.Conn` 开放 AUTH，交给它的连接不能再包一层
- **审计**：AUTH 成功（`smtp.auth`）、失败（`smtp.auth.reject`，带原因）、每次提交（`smtp.submit`：
  信封发件人、收件人数、字节数、回复码、outbox / 草稿 id）都进 `actions`，带 `via: "smtp"`、
  客户端地址和经过的代理，从不记密码和正文。失败时用户名只在形如凭据 id 时才记，填反了字段的客户端不会把密码写进日志；
  被限速的尝试每个客户端十分钟只记一条
- **停机**时等进行中的会话做完，和 HTTP 共用 30 秒宽限，超时才断。在 `DATA` 中途被断的会话没拿到 `250`，信仍在客户端手里
- **配置**：不设监听地址就不开；给证书文件时 yuuka 自己终结 TLS，不给就必须配代理名单。
  证书文件变化后一分钟内换上新证书（[`mailnet.CertFiles`](../../internal/mailnet/certfile.go)），
  续期不用重启；新文件读不出来时继续用旧的，下一次检查再试。变量见[配置参考](../operate/config.md#smtp-提交口)，
  网络上怎么摆见[运维：SMTP 提交口](../operate/submission.md)

**监听方式**（对 yuuka 所有的邮件协议监听器都成立，包括 M5 可能加的 IMAP 网关）。
实现在 [`internal/mailnet`](../../internal/mailnet/mailnet.go)：它包住一个 TCP listener，
`Accept` 只交出已经过检查的连接，`RemoteAddr` 是真实客户端，`mailnet.Proxy` 告诉审计它经过了哪个代理。

- **只做隐式 TLS**：提交口用 465，IMAP 用 993（RFC 8314 推荐的形式），**不做 STARTTLS**。
  隐式 TLS 一建连就是 TLS，任何 TCP 层的 TLS 代理（Caddy layer4、Traefik、HAProxy）都能终结它，
  证书可以和 HTTPS 一起由代理统一管理；STARTTLS 要在协议内部升级，通用代理处理不了，只能由 yuuka 自己做。
  yuuka 也可以自己终结 TLS，两种部署都支持。代理终结 TLS 时 yuuka 收到的是明文，
  所以**明文只接受来自代理的连接**；yuuka 不终结 TLS 又没配代理时拒绝启动。其他来源都到不了明文 AUTH
- **支持 PROXY protocol**（v1 / v2）：放在代理后面时，yuuka 只看得到代理的地址，
  提交口的限速、审计和 `actions` 里的来源就全失真了。只接受配置里列出的代理地址发来的 PROXY 头，
  其他来源带 PROXY 头一律断开——否则任何人都能伪造自己的来源地址
- **来自代理的连接必须带 PROXY 头**，没有就断开。可选的话，代理漏配 `send-proxy` 时所有客户端
  都会被悄悄记成代理自己——限速塌成一个桶，审计也错，而且没人会发现。
  代理自己发起的连接（健康检查）用 v2 的 `LOCAL` 或 v1 的 `UNKNOWN`，来源记为代理本身。
  代价：和代理同地址的服务不能绕过代理直连这个口，要么也走代理，要么换个地址
- **代理名单单独配置**，不复用 HTTP 的 `YUUKA_TRUSTED_PROXIES`，语法相同（同一个解析器）。
  两者语义不同：`X-Forwarded-For` 是可选的、逐跳往回走，PROXY 头是强制的。共用一份名单，
  HTTP 反代的地址就再也不能直连提交口，只做 TCP 转发的代理又会被 HTTP 那边平白信任——两个方向都扩大了信任。
  常见的「一个 Caddy 同时做两件事」只是把同一个地址写两遍。提交口的名单是 `YUUKA_SUBMISSION_PROXIES`
- **头和 TLS 握手在 `Accept` 之前做完**，每个连接一个 goroutine，共用一个超时（默认 10 秒）。
  不在第一次 `Read` 时才懒解析：SMTP 是服务端先说话，懒解析意味着问候语在检查之前就发给了对方，
  `RemoteAddr` 也得变成会阻塞的调用。现在一个只发半个头就不动的对端只占自己的 goroutine 到超时，
  不耽误别人的 `Accept`；服务端看到的全是过了检查的连接。`Close` 会中断还在握手的连接
- **解析用 [`go-proxyproto`](https://github.com/pires/go-proxyproto)，策略在 yuuka**。线格式不大，
  起初是自己写的；换掉它是因为这个库带着只有被广泛部署才会学到的东西：给客户端证书 TLV 留的余量
  （v2 长度上限 4 KiB，自己写的版本设了 1 KiB，会拒掉合法的头）、LOCAL 下未定义的 family 字节也拒绝、
  修过的可被远程触发的 panic，而 Traefik 的发送端用的就是它。它只依赖 `x/net`。
  留在 yuuka 这边的是：哪种错误对监听器意味着什么、只接受 TCP 来源（库也解析 AF_UNIX 和 UDP，
  放在 TCP 邮件监听器前面没有意义，拒绝比猜一个地址好）。随库带来的两处行为：
  PROXY 命令配 UNSPEC 拒绝（接受就等于把客户端记成代理本身，和上一条要防的是同一件事）；
  v1 头没能一次读进缓冲区就拒绝，不等——代理总是一次写完整个头，等下去只给拖延的对端占 goroutine 的机会。
  v2 头不完整时照样等到超时。解析经 fuzz 测试

各种组合的结果（「代理」= 配置名单里的对端；测试表 `TestCombinations` 与此一一对应）：

| yuuka 终结 TLS | 对端 | 先收到的 | 结果 |
|---|---|---|---|
| 是 | 非代理 | TLS ClientHello | 接受，来源 = TCP 对端 |
| 是 | 非代理 | PROXY 头 | 断开（`ErrUntrustedHeader`） |
| 是 | 非代理 | 其他明文 / 什么都不发 | 断开（`ErrPlaintext` / 超时） |
| 是 | 代理 | PROXY 头 + ClientHello | 接受，来源 = 头里的客户端 |
| 是 | 代理 | `LOCAL` / `UNKNOWN` + ClientHello | 接受，来源 = 代理本身 |
| 是 | 代理 | 没有 PROXY 头 | 断开（`ErrMissingHeader`） |
| 是 | 代理 | PROXY 头 + 明文 | 断开（TLS 握手失败） |
| 否 | 非代理 | 任何东西 | 立即断开，一个字节都不读（`ErrPlaintext`） |
| 否 | 代理 | PROXY 头 + 明文 | 接受，来源 = 头里的客户端 |
| 否 | 代理 | `LOCAL` / `UNKNOWN` | 接受，来源 = 代理本身 |
| 否 | 代理 | 没有 PROXY 头 / 什么都不发 | 断开（`ErrMissingHeader` / 超时） |
| 否 | 未配置任何代理 | — | 拒绝启动 |
| 任意 | 代理 | 畸形、超长或截断的头 | 断开 |

一种组合 listener 认不出来：yuuka 不终结 TLS、代理却把 TLS 原样转了过来。明文 SMTP 是服务端先说话，
listener 不能等客户端先发字节，这种配错会在协议层表现为客户端握手失败，不会被当成明文接受。

好处是**不用改任何现有软件**：监控、告警、定时任务，几乎所有服务端软件都会说 SMTP。
配一个账号密码就行。

副作用是个很划算的赠品：yuuka 顺带成为同一部署环境里所有服务的**统一发信网关**——
所有服务发出的信都进同一个 Sent 归档、共用一套限速与重试、
身份策略集中、凭据可单独撤销。比每台机器各自存一份 SMTP 凭据好得多。

写 HTTP 的服务也可以走 REST/MCP + token，两条路并存。那条路径上信头 `From` 由系统按 identity 组装，
调用方写不进去，上面的校验天然成立。

### 13.7 认证边界：每个端点自己认证

yuuka 不假设自己部署在什么网络里（§2）。所以**每个端点都由 yuuka 自己认证**；
反向代理、身份代理、VPN 这类网络层保护可以加，但只算纵深防御——
拿掉它们，不能让任何一个端点变成无认证。

| 端点 | 认证方式 | 需要从哪里可达 |
|---|---|---|
| WebUI | 内置登录 → `session` 凭据 | 运维者的浏览器 |
| ingest（Worker → yuuka）| HMAC + 时间戳（§2） | Cloudflare |
| SMTP submission | SASL → `smtp_password` 凭据 | 发信的服务；默认只在私有网络可达，发信方在私有网络外时经独立中转机（见下） |
| REST / MCP | Bearer → `token` 凭据 | 调用它的 agent 和服务 |
| TG 回调按钮（§8.4） | 长轮询，没有入站端点。校验 `callback_query.from.id`、`chat.id` 和回调数据 `w:<ULID>` → 记为 `YUUKA_TELEGRAM_OPERATOR` 指定的人，`credential_id` 为空、`via: telegram`。**渠道进库之后（§17，[`notify.Manager`](../../internal/notify/manager.go)）**：每次按都重新读库，必须同时对上 bot、`chat.id`、`from.id`、一条未撤销的绑定和一个启用中的人 → 记为**绑定的那个人**，`credential_id` 为空、`via: telegram`、带 `binding_id` | 只能撤回扣留中的信；见下 |
| `GET /healthz` | **不认证**（唯一的例外） | 反向代理的健康检查 |

这几个端点各自认证，不要混。

**提交口为什么默认不上公网**：它和 HTTP 端点不一样，躲不到 Cloudflare 的代理后面——橙色云只代理 HTTP(S)，
提交口的主机名只能直接解析到接收连接的机器。开在 yuuka 这台机器的公网地址上，就公开了源站 IP，
Web 和 webhook 靠 Cloudflare 得到的那层保护随之失效；连接上限和认证限速（§13.6）防得住暴力破解，
防不住流量型攻击。所以按发信方在哪里选：都在自己的私有网络里，就只在那里监听；有发信方在外面，
就让一台独立的中转机承受公网，经私有链路加 PROXY 头转给 yuuka，最好原样转发 TLS、不在中转机上解开。
直接开在同一台机器上只适合本来就没有 CDN 要保护的部署。三种做法的配置与取舍见
[运维文档的「先选暴露范围」](../operate/submission.md#先选暴露范围)。被打期间不丢信：收信由 Worker 落 R2、reconcile 补齐（§2.1）。

**`/healthz` 为什么可以不认证**：反向代理的主动健康检查拿不出任何凭据，给它发一个 token
就得在代理配置里存一份长期凭据，比开放一个只答一位的端点更糟。它能开放，是因为它只回答
「进程在、能在短超时内 ping 通 Postgres」：200 或 503，正文是常量，不带版本、错误文本和耗时，
不查任何邮件数据、不写库、不按请求记日志；并发探测共用同一次 ping、约每秒最多一次，
刷它占不住连接池。代价是外人能知道这里跑着一个服务、它此刻是否连得上库——端口开着本来就暴露了前者，
后者对攻击没有增量。这几条约束一旦放松（比如想加版本号或依赖细节），它就不再是一个可以不认证的端点，
该改成带凭据的独立端点。路由清单与各自的认证方式在
[`cmd/yuuka/main.go`](../../cmd/yuuka/main.go) 的 `routes`，清单测试保证新端点不会无意中漏掉认证。
WebUI 的页面壳（静态资源）同样不需要会话，但它不含数据，数据全走 `/api/`。
`/api/v1/openapi.json` 也不需要：它是接口描述，和公开仓库里的那份一样（§5.1）。TG 容易被当成纯输出而漏掉，但它上面有按钮。
M3 的按钮只有撤回（§8.4）：最坏的后果是信发不出去，所以记作配置里的人、不发凭据就够了，
和 CLI 一样不冒充登录；将来出现「确认发送」按钮时它就是 `submission:send` 的入口之一，那时再给它一种可吊销的 `session` 类凭据。

**渠道进库之后（§17，已实现；CLI 一侧是 `yuuka channel bind`，WebUI 一侧是设置页的「我的通知」），「谁按的按钮算数」由绑定决定，而绑定只能经 `/start` 码加双向确认建立，不手敲 id。**
手敲的 chat id 和用户 id 没人核对：打错一位，撤回按钮就交给了一个陌生人。流程：

1. CLI 或带再验证的 WebUI 会话生成一个绑定码（16 个随机字节、10 分钟有效、库里只存哈希），
   人拿到 `https://t.me/<bot>?start=<码>`（群用 `startgroup=`）。WebUI 只能给自己发起绑定，给别人绑定在主机上做
2. bot 收到 `/start <码>`，在码上记下 `from.id`、`chat.id` 和名字，回复「等你在 yuuka 里确认」。这一步什么都不授予：
   码只证明「拿到链接的人在这个 chat 里」，不证明他是谁
3. 人在 yuuka 里看到这个确切的 Telegram 身份（名字、用户 id、私聊还是群）后确认：WebUI 仍在再验证的有效期内，或 CLI 在终端上问 y/N。
   这一步才建绑定；`security` 告警发到这个人的**其他**绑定和管理员的绑定（§8.4），bot 回复已绑定

码被别人截走、抢先按了 `/start`，第 3 步看到的就是那个人的身份，确认不下去；确认的一方必须是 yuuka 里的人，这是双向的意思。
`/start` 由 `serve` 的轮询者收（一个 bot 只能有一个轮询者，§17.4），所以 CLI 的 `channel bind` 不自己轮询 Telegram，
而是等库里的码被兑现；`serve` 没在跑、或者包装器不可用（没有 KEK）时绑定完成不了。唯一不经这个流程的绑定是从环境变量的一次性导入（§17.7）。

按钮的认证每次都现查：bot、`chat.id`、`from.id`、未撤销的绑定、启用中的人，全部对上才执行，不缓存，撤销绑定立即生效。
撤回以绑定的那个人的身份、走和 WebUI 相同的存储函数，`actions` 里记 `via: telegram` 和 `binding_id`，`credential_id` 照旧为空。
绑定本身是可撤销的行，已经承担了「凭据」的撤销语义；等第一个能发信的按钮出现时，再加一种凭据类型（§8.4）。把 REST / MCP 放在只认浏览器 cookie 的身份代理后面是个常见错误——
agent 不是浏览器，拿不到那个 cookie。

**WebUI 的内置登录**：

- 单一运维者，首选 **passkey（WebAuthn）**：没有密码可泄露，也不需要邮件找回——
  邮件系统自己的登录不该依赖收邮件
- 第一个 passkey 由 CLI 签发一次性注册链接（`yuuka admin enroll`），之后在 WebUI 里添加更多设备
  （先用已有的 passkey 再验一次，§13.10）；
  所有 passkey 都丢了，同样用 CLI 恢复。能在机器上跑 CLI 本身就是最高权限
- **不提供密码 + TOTP 作为备用登录**。备用方式就是整条链上最弱的一环：
  攻击者只需要走它，钓鱼页实时转发密码和 TOTP 就能绕过 passkey 的防钓鱼能力
- 唯一的缺口是「passkey 全丢、又碰不到服务器的 shell」，用**离线恢复码**补：
  - `yuuka admin enroll` 同时打印一组（约 8 个）一次性恢复码，运维者离线保存，库里只存哈希
  - 恢复码只能在专门的恢复入口使用，作用只有一个：**发起一次新 passkey 的注册**。
    它本身不建立会话、不能读信，偷到恢复码也不等于登录
  - 用一个作废一个；每次使用记入 `actions` 并推送 TG 告警；恢复入口严格限速
  - 和密码 + TOTP 的区别：它从不出现在日常登录页上，没有可供钓鱼的常规路径；
    每个只能用一次，被人用了运维者会立刻知道
- 可选「信任前置身份代理」：部署在 Cloudflare Access、oauth2-proxy 这类代理之后时，
  可以把代理的身份断言映射成运维者的 `session`。但**必须验签**（例如 Access 的 JWT 对照它的 JWKS），
  不能只信一个请求头；验签失败就拒绝，而不是回落成无认证。M1 没有实现它

M1 的落法（`internal/authn`），以及设计里没写死、实现时定下的几处：

- **只有 passkey 登录产生会话**。注册链接、恢复码、注册 passkey 都不发会话：
  恢复码换一个短时效的一次性注册令牌（和 `admin enroll` 签发的是同一种，表 `enrollment_tokens`），
  令牌换一次 passkey 注册，之后照常用新 passkey 登录。链路只有一条，「恢复码不等于登录」由结构保证
- 注册链接是 `<origin>/enroll#<token>`：放在片段里，不进服务器和代理的访问日志，
  由 WebUI 页面读出后提交给注册接口（路由见 [`internal/authn/handler.go`](../../internal/authn/handler.go)）。令牌在 begin 时校验、
  在 finish 时与写入 passkey 同一事务里原子地消耗，所以并发的两次注册只有一次成功
- 会话：不透明随机串，库里只存哈希；cookie 用 `__Host-` 前缀、`HttpOnly`、`SameSite=Strict`，
  有绝对有效期、不滑动续期（再登录只是摸一下验证器）；登出即撤销那一行。
  cookie 属性、有效期和 `last_used_at` 的写入节流见 [`internal/authn`](../../internal/authn/authn.go)
- **CSRF**：`SameSite=Strict` 让跨站页面根本带不上 cookie；第二层是 `net/http` 的
  `CrossOriginProtection`（先看 `Sec-Fetch-Site`，再看 `Origin` 对 `Host` 或配置的 origin），
  作用于 `/auth/` 下所有写请求和所有以 cookie 认证的写请求；第三层是 POST 一律要求
  `application/json`，HTML 表单跨域发不出来、`fetch` 跨域发它要先过 yuuka 从不应答的预检。
  Bearer token 不是环境凭据，不需要这些。代价：从邮件或 TG 点链接进 WebUI 的第一次顶层导航不带 cookie，
  WebUI 要按「页面壳不需要会话、数据走同源 fetch」来写
- WebAuthn 要求可发现凭据（无用户名登录）和用户验证；不要 attestation。签名计数倒退
  （可能是被克隆的验证器）直接拒绝登录并告警。每注册一把新 passkey 都推送告警，不只是恢复时
- 限速：恢复入口全局限速（额度见 `internal/authn`）；登录与注册按客户端地址限速，审计里的 `remote` 也是它。
  默认客户端就是 TCP 对端，`X-Forwarded-For` 一概不看（§2 不假设前面有什么，谁都能写这个头）。
  代价是在代理之后所有人共用代理的地址：按地址的限速退化成一个桶，刷爆它就能把运维者挡在登录外，
  审计也只记到代理。所以可以用 `YUUKA_TRUSTED_PROXIES` 显式列出代理的网段：只有对端在其中时才读这个头，
  并且**从右往左**跳过可信的代理，取第一个不可信的地址——最左边那项是客户端自己写的，从不直接采信；
  遇到格式不对的项就退回对端地址，免得客户端靠发垃圾给自己换桶（[`internal/clientip`](../../internal/clientip/clientip.go)）。
  这只在代理自己也按同样规则写这个头时成立：代理前面若还有 CDN，代理要把 CDN 的网段配成它的可信代理。
  恢复入口始终全局限速，被刷爆时运维者仍有 CLI
- 进行中的 WebAuthn 仪式（challenge）放在内存里，键是一个 `HttpOnly` cookie，用一次即删。
  重启只意味着重试一次；哪天要多副本部署再挪进 Postgres
- **主机上的 CLI 不登录，也不冒充任何人**。`yuuka admin enroll`、`yuuka prefix`、`yuuka token`
  在服务器上跑，能跑就是最高权限，但 yuuka 并不知道是哪个人在敲：它们写进 `actions` 的
  `actor` 是 `system`、`credential_id` 为空；管理命令（`prefix`、`token`）还在 `args` 里带 `via: "cli"`
  和执行它的系统用户名 `host_user`。
  记成 `human:<operator>` 会凭空制造一次没有发生过的登录；多人共用一台主机时，`host_user` 才是真实可查的那一条线索
- WebAuthn 的 RP ID 与 origin 来自配置（[`cmd/yuuka/config.go`](../../cmd/yuuka/config.go)），
  不从请求里推断：origin 必须是 https，或 http://localhost

### 13.8 ACL 的粒度到账户为止

如果某些内容不该让某个主体看到（例如受合规约束的账户不该进 agent 的上下文，§9.4），
用**账户级**开关表达，不要下沉到单封信：`account_access` 每行是（账户, 主体, `can_read`）。
表在 [0003](../../internal/store/migrations/0003_identity.sql) 建、[0005](../../internal/store/migrations/0005_sending.sql) 补外键，
M3.5 起每条读路径都按它过滤（条件只写在 [`read.go`](../../internal/store/read.go) 的 `readableAccount` 一处）。

**默认语义（M3.5，已实现）**：每个账户有一个 `accounts.default_read`（[0019](../../internal/store/migrations/0019_pull_accounts.sql)），没有行的主体按它；
有行就按行，`can_read` 为真为假都覆盖默认。Cloudflare 账户默认可读，拉取账户默认不可读，加账户的那个人自动得一行可读
（[`store.AddPullAccount`](../../internal/store/pull.go)；`SetAccountAccess`、`SetAccountDefaultRead` 是 `account access` 的存储一侧）。
过滤已经打开，升级前后的对照是 [`TestDefaultReadUpgrade`](../../internal/store/access_test.go)：
0018 的部署（一个 Cloudflare 账户、人、agent、服务、收到和发出的信、没有行）升级后每个主体看到的和以前一样；
新加的拉取账户只有加它的人看得到；一行 `can_read=false` 藏住默认可读的账户，删掉这行回到默认。
落地时定下的两处：

- **系统主体读全部账户**。它是主机本身：主机上的 CLI 和评分器都以它的身份读（§13.10 的信任根，能跑 CLI 的人本来就能读库），
  它从不持有凭据（token 只签给 agent 和 service，人只用 passkey）。不这样，主机上的命令和评分器会看不到拉取账户的信，
  而评不评分该由 `YUUKA_TRIAGE_ACCOUNTS` 决定（§10.1.4），不该由一行可见性顺带决定
- **不带合法主体 id 的 Viewer 只读默认可读的账户**。它不指向任何主体，也就不会有行，按「没有行就按默认」算；
  拉取账户对它仍然关着。这样的 Viewer 只出现在测试里，过滤打开前它们读得到全部

- **为什么要默认值**：store 里的过滤已经写好、默认关着，打开后的语义是「有一行 `can_read` 才可读」。
  照这个直接打开，现有的人和 agent 一封信都看不到，以后新签的 agent token 也看不到 Cloudflare 账户。
  `default_read` 让已有的部署升级前后看到的一样：迁移给已有的账户 `true`，然后去掉列的默认值，新账户必须说明自己的
- **为什么行能往两个方向覆盖**：「这个 agent 读不到 Cloudflare 账户」和「这个人读得到某个拉取账户」都是真实需求，
  只能加权限的行表达不了前一个
- **没有选「人隐式全可读、非人要行」**：更简单，但多 human 的部署里，其他人会读到某个人接进来的工作邮箱。
  拉取账户默认不可读、加的人自己得一行，让「接进来一个邮箱」不顺带把它交给别人
- 打开开关是 M3.5 里唯一会改变现有部署可见性的改动；默认值选错，agent 会突然看不到信，或者突然看到不该看的邮箱。
  所以这一步单独一个提交，带「已有部署升级前后谁能读什么」的对照测试

**单封信级别的 ACL 一律不做。** 一旦允许"这封信某主体看不到"，
就得在每个查询、每个聚合、每次搜索上带 ACL 过滤，复杂度跳一个台阶，
而且很容易出现"搜索结果数对不上"这类难查的 bug。账户级粒度足够表达真实需求，
且天然和 §9.2 的 `sync_policy` 对齐。

#### 可见性只算一次，所有读路径共用

账户级粒度省掉的是「每封信一份 ACL」，省不掉「每条读路径都要过滤」。
跨账户模型（§9.1）下，漏洞不在 `messages` 本身，而在那些顺手聚合、顺手推送的地方。所以：

- **可见性在一处定义**：`visible_deliveries(principal)` = 该主体有 `can_read` 的账户上的投递；
  一封 message 可见，当且仅当它至少有一条可见投递。**任一可读投递即可读**内容，
  但该主体看到的 `deliveries` 只含可见的那几条，`raw` / `auth` 也只给可读那一路。
  我们发出的信没有投递，按发信 identity 所属的账户算：Sent 副本所在 outbox 的 identity 的账户可读，这封信就可读（§4.6）；
  同一内容既投递到不可读的账户、又从可读账户的身份发出时，只经 Sent 副本可见，投递、收件前缀、认证都不给。
  三条（投递、outbox、消息）都由「账户可读」这一个条件推出来，写在
  [`read.go`](../../internal/store/read.go) 的同一处
- **store 层强制**：每个读查询函数都要求一个 `Viewer` 参数，没有它就调不了。
  过滤靠类型系统保证存在，不靠每个调用点记得加 `WHERE`
- **所有读路径都过这一道**：

| 读路径 | 受限主体看到的 |
|---|---|
| 搜索 / `search_messages` | 结果和**计数、分面**都只在可见消息上算；条件（含否定）只读可见的投递和 outbox 行（§16.4） |
| `get_message` / 附件 / 结构化视图 / 消息详情的 To、Cc、Reply-To | 消息可见才可取；附件随消息；收件人头部从可读那一路的原件解析，只经 Sent 副本可见的信给的是 Sent 副本的头 |
| `extractions` / `get_otp` | 只来自可见消息 |
| 线程 | `n`、`participants`、`first_at` / `last_at`、`snippet` 都按可见消息**现算**；`threads` 表里存的聚合值只服务不受限的视图 |
| `list_prefixes` / 统计 | 只在可见投递上聚合 |
| 前缀详情 / 泄漏检测（§3.3） | 观测到的发件域、泄漏 / 仿冒计数和列出的信都只来自可见投递：看不见的账户里谁在发信也是头部 |
| `changes`（[`Changes`](../../internal/store/changes.go)，REST `/changes`、`changes_since`）/ 以后的 SSE、JMAP `queryChanges` | 目标不可见的事件跳过，游标照常前进；可见与否按目标对象自己的规则（§8.6），已消失的对象只给能看到全部的主体 |
| `actions` / `proposals` | 审计日志只给 human 会话；agent 只看自己发起的 |
| 提案（`Proposals`、`ProposalByID`，REST `/proposals`、`list_proposals`） | 人看全部，agent / service 只看自己提的；关于某封信的只在那封信可见时可见。理由和引用会引原文，可能是读者看不到的信 |
| 评分（`LatestScore`，§15.2；消息详情的 `triage_score`、视图的 `facts.triage`） | 消息可见才可取；评分的输入记的是投递事实，看不到这封信全部投递的主体拿不到输入，REST 另外只给有 `delivery:read` 的人，agent 视图从不带输入 |
| 评分的倾向与过滤（`TopAnswers`、`triage=`，列表、线程、搜索、`search_messages`）、报告（`ScoreCounts`，`yuuka triage report`、`/triage/report`） | 只在可见消息上取、过滤和计数；报告只给人 |
| 笔记（`Notes`、`NoteByID`，消息详情的 `notes`、`get_message`，§8.10） | 在看得见的信上，人和读得到全部账户的主体看全部笔记，其他主体只看自己写的（`noteVisible`）：笔记的正文和引用可能引到读者看不到的信；线程笔记按锚点那封信可见与否算。`get_message` 给的和 REST 相同。删除另算：人能删他看得见的任何笔记，其他主体只删自己的。删掉的笔记在 `changes` 里只给人 |
| 标注来源（`MessageAnnotations`，`GET /messages/{id}/annotations`） | 消息可见才可取；它是审计日志的一部分，agent / service 只看自己的 action 写的那几行 |
| 撤销（`Undo`） | agent / service 只能撤销自己的 action，别人的和不存在一样；看不见的信上的行不动、只计数 |
| 标签列表（`Labels`）、`label=` / `archived=` 过滤 | 只在可见消息上计数和过滤 |
| 规则预览、草拟（`PreviewRule`、`FactsFor`、`CountMatches`） | 只在可见投递上求值和计数；只经 Sent 副本可见的信没有投递，不能拿来草拟 |
| 审核结论里指向的信的主题（`Subjects`，草稿和 outbox 行的 `review`） | 结论只存 message id；主题只在那封信对读者可见时给，看不见的只剩 id |
| 侧栏计数（`Counts`，REST `/counts`） | 每个数和它对应的列表同一个条件：outbox 行按发信身份的账户、提案按 `proposalVisible`、草稿按 `visibleDraft`；outbox 的数另要 `delivery:read` |
| 账户列表（M3.5：MCP `list_accounts`、REST `GET /api/v1/accounts`） | 只列可读的账户，只给名字、种类、地址；来源的主机、用户名、同步状态只在人的管理端点上（§13.10） |
| 按账户搜索（M3.5：`account:`，§16.3） | 只匹配可见的投递；对不可读的账户和不存在的账户结果一样是空 |
| 推送通知 | 只发给 human，不经过受限主体。渠道进库之后（§17）绑定属于某个 human，`mail.new` 只在这次投递对**绑定的那个人**可见时才推（`DeliveryVisible`）；扣留卡片按 outbox 行、升级卡片按草稿、熔断卡片按身份所在账户的可见性（§8.4） |

测试上用一个「含受限账户」的固定夹具，表驱动地把上表每条读路径跑一遍，断言没有泄漏。
新增读路径就是往这张表加一行。store 层的这张表是
[`TestVisibility`](../../internal/store/sent_test.go)：两个账户都不默认可读，每个主体能读什么只来自它的行，
用可读、不可读两个账户下的收信和 Sent 副本跑列表、搜索、详情、线程、前缀视图和账户列表（`VisibleAccounts`）；
变更日志在同一个夹具上对每条变化断言「列出 ⟺ 目标对象自己的读路径可见」。
REST 层的 [`TestACL`](../../internal/api/api_test.go) 管的是另一半：它遍历路由表（§5.1），每个端点要认证、要 scope、把认证出的主体交给 store；
只有人能调的端点（决定提案、管理规则、`outbox.resolve`）另断言持有全部 scope 的 agent 得到 403，且请求到不了 store。
规则本身是运维者的配置，读它不带 Viewer；能读的只有人，这由端点保证。凭据、身份、授权的列表同理，端点只放人的会话（§13.10）。

反过来，store 里不带 `Viewer` 的方法不少：ingest 和 outbox 执行器用的（`Unprocessed`、`ClaimOutbox`、`RecoverAttempts`……），
还有上面那些只给人的配置读写。它们的安全目前靠两条：调用方是系统批处理，或者端点只放人的会话。
这两条都还只是约定，代码里没有一份清单。gofer 用一份文档列出每个不受 ACL 约束的方法和它唯一可信的调用方（§1.2）。
本项目更想让测试来管：把这类方法列成一张表，测试断言 REST、MCP 的处理函数
只经过表里登记为「人可调」的那些，新增一个不带 `Viewer` 的方法就要在表里写明谁能调。
原定随 M3.5 打开过滤一起做；过滤打开时（P1）还没有新的 REST、MCP 处理函数，这张表随拉取账户的 REST 和 MCP（P6）一起建，
`internal/store/pull.go` 里来源、游标、失败表的方法都不带 `Viewer`，是它的第一批条目。

发信授权读的事实也走同一个条件：回复的默认身份（`ReplyIdentities`）和线程参与者（`ReplyFacts`）
只来自可见的投递、可见的消息和可见的 outbox 行，同在 `TestVisibility` 里。
线程通信方包含 To / Cc / Reply-To（§8.9），这些是头部数据：看不见的信上的地址不能经授权决定
（「这个收件人放不放行」）透露给受限主体，测试里专门断言了这一条；判断头部可不可信也只看可见的投递。
outbox 本身的读路径（`OutboxList`、`OutboxByID`，REST 的 `/api/v1/outbox`）也按这一条过滤：发信身份的账户可读才可见；行上的主题取自它的 Sent 副本，同一个条件保证读得到这条副本。
TestACL 对草稿类端点另断言：别的 agent 即使持有全部 scope，拿到的也是和「不存在」一样的 404。
草稿行另有一条 M2 规则（[`drafts.go`](../../internal/store/drafts.go) 的 `visibleDraft`，唯一的一处）：
human 看全部，其他主体只看自己写的。草稿是没发出去的字，常常是替人写的，比信件收得更窄。

草稿和 outbox 的变更事件（`changes` 的 `draft`、`outbox` 两类）随 M3 的游标拉取定下了：
和读这两样对象本身是同一个条件——草稿按 `visibleDraft`（作者和人），outbox 按发信身份所属账户。
变更只是「去读一下」的提示，比对象本身收得更窄或更宽都会让两边对不上。

**M3.5 定下的一件事**：agent 读不了某个账户时，也看不到它自己以该账户下的身份发出的信。
§4.6 的规则只按账户算、只在一处；为「自己发的」开一个例外，就多一条要进上表的读路径，还要回答「自己发的」按草稿作者、outbox 的提交者还是身份 owner 算。
agent 要看，就给它读那个账户。

#### 存在性是有意披露的

受限主体能知道受限邮件**存在、有几封**，不能知道它们的任何头部或正文。这是一条明确的策略：

- 线程里显示「另有 k 封在你无权的账户中」，不显示发件人、主题、时间
- `changes` 的全局序号会因跳过的事件出现空洞，同样只透露「有活动」

理由：受限的主体实际上是 agent 和服务，限制的目的是让合规邮件的**内容**不进它们的上下文，
而不是对它们隐瞒这个账户存在。让计数悄悄对不上反而会诱发错误推理。
完全不可见（连存在都不披露）是非目标：它要求按主体维护各自的变更序列，代价与收益不成比例。

### 13.9 对既有设计的影响与落地顺序

- §8.8 的 `actions.actor` → `principal_id` 外键
- §8.2 的能力分级 → §13.4 的 scope 枚举
- §9.3 的 identity → transport 之外，增加 identity → principal 授权
- §11.1 的署名分层 → 读 `principals.display_name`

落地顺序：

| 阶段 | 内容 |
|---|---|
| M1 | **`principals` / `credentials` / `identity_grants` / `account_access` / `actions` 建表**，WebUI 内置 passkey 登录、走 session 凭据（M1 只有 JSON 端点，页面在第二阶段）；只读 MCP 与 REST 的 bearer token，由 `yuuka token` 签发 |
| M2 | SMTP submission 口（跟发信能力一起做）；`accounts` / `identities` 建表（§9.5），`identity_grants`、`account_access` 补外键（已落地：[0005](../../internal/store/migrations/0005_sending.sql)） |
| M3 | 管理动作进 WebUI（§13.10）：跟着信走的决定、权限类配置的查看与收紧；放宽权限当场再验 passkey |
| M3.5 | `account_access` 生效：`accounts.default_read` 加行覆盖，打开读路径的过滤（§13.8，跟多账户一起做；已落地） |

`identity_grants` 和 `account_access` 在 M1 只建表、不读：外键指向的 `identities`、`accounts`
当时还不存在，这两列是不带外键的文本。M2 的迁移建了这两张表并补上外键：`account_access` 里原有的账户名
换成账户 id；`identity_grants` 不可能有合法的行（没有 identity 可指），有行时迁移拒绝执行、
要人先清掉，而不是静默丢掉一条授权。

主体与凭据的表从 M1 就建，因为主体模型是改起来最贵的部分。
M0 不需要这一层——那时只有一个人和一个脚本。

### 13.10 管理面：收紧一步完成，放宽先再验 passkey

M2 结束时，几乎所有管理动作都只能在主机上敲 CLI。这不全对，但也不该反过来做一个和读信分开的「管理后台」。
管理动作按性质分三类，各有各的位置：

| 类 | 例子 | 在哪做 | 谁能做 |
|---|---|---|---|
| 信任根与主机运维 | `admin enroll`、恢复码轮换、`migrate`、`reconcile`、`reparse` | 只在主机 CLI | 能登上主机的人 |
| 跟着信走的决定 | 认领前缀、写期望域、规则、改判、`unknown` 外发的裁决 | WebUI，放在引出它的信或前缀旁边（§12.12）；CLI 保留 | 按 scope（`prefix:manage`、`rule:manage`……），和其他写端点一样 |
| 改变谁能做什么 | 凭据、身份、授权 | WebUI：看和收紧一步完成；签发、添加、调高要先再验一次 passkey。CLI 保留 | 只认 human 的 `session`，且带 `admin`；放宽另要会话在 5 分钟内验过 passkey |

**信任根留在主机。** §13.7 的恢复链建立在「能跑 CLI 就是最高权限」上：WebUI 起不来、passkey 全丢、
登录入口被刷爆时，恢复不能依赖 Web。迁移和重建是主机上的运维动作，不做成 HTTP 端点：
一个能触发全量 `reparse` 的端点，被会话劫持后就是一个现成的放大器。

**跟着信走的决定放在信旁边。** 做这类决定，起因都是读信时看到了什么：一个出事的前缀，一类反复来的通知。
让人切到 shell 敲命令，等于先把上下文丢掉，再凭记忆找回来。这类动作不改变任何主体的权限，
所以和其他写端点一样只看 scope。CLI 保留：规则的 `--spec` 文件可以进版本库，脚本也要用。
界面和 CLI 调同一个 store 函数，校验只写一份，和发信只有一个提交核心（§8.9）是同一个道理。

**改变权限的配置不对称。** 吊销凭据、调低 `send_policy` 或限额、删授权、停用身份，只会让系统更保守，
误操作的代价是某个 agent 报一次缺权限，所以在界面上一步完成。签发 token、调高档位或限额、加身份，
会扩大某个主体能做的事：会话一旦被劫持，攻击者靠它就能留一个比会话活得长的后门，
比如一把 `submission:send` 的 token 加一条 `auto` 授权。

M3 起初把这类操作只留在 CLI。代价落在每个运维者身上：没有身份就发不了信，第一次配置要在浏览器和 shell 之间来回切，
而要防的只是「偷到会话 cookie」这一种情形。挡住它不需要主机，只需要证明此刻坐在浏览器前的人还握着 passkey。

**放宽：当场再验一次 passkey，之后 5 分钟有效。** 放宽权限的端点要求会话在 5 分钟内做过一次 passkey 断言
（要求用户验证，只接受这个人自己的 passkey），否则回 `403 elevation_required`，界面弹出 passkey 后重试。
单靠偷来的 cookie 过不了这一步；要放宽，攻击者还得同时握着验证器、通过它的 PIN 或生物识别。取舍：

- **窗口而不是一次一验**。第一次配置通常是「加身份、写授权、签 token」连着三步，一次一验要碰三次验证器，
  而三次断言防不住的东西一次也防不住：能在窗口里用会话的人，多半也能在下一次弹窗时诱导点一下。
  5 分钟够一次配置做完，又短到之后偷到的会话发现窗口已经关了
- **窗口记在会话那一行上**（`credentials.elevated_until`，[0015](../../internal/store/migrations/0015_elevation.sql)），
  不放内存：重启和多进程看到同一个答案；登出撤销这一行，窗口随之结束，不需要单独的生命周期。
  只有 `session` 能有这一列，token 不论 scope 都不是坐在浏览器前的人
- **每次放宽都推送告警**，和注册新 passkey 一样。被盗的提权会话靠它被发现：告警写清谁、在哪个会话、放宽了什么。
  收紧不告警，免得告警多到没人看
- **添加 passkey 也要先验一次**。新 passkey 比会话活得更久，是最大的后门；原先只要求会话，
  意味着偷到 cookie 就能给自己注册一把 passkey。现在它和其他放宽走同一道门
- **防不住的**：同时拿到会话和验证器（比如设备本身被控制），以及窗口内页面里跑起来的脚本。
  后者靠邮件正文永远在不带 `allow-scripts` 的 sandbox iframe 里（§12.2）来挡，不靠这道门

**权限类端点只认人的会话。** scope 挂在凭据上，`admin` 也能被 `yuuka token issue` 写进一把 token。
如果这类端点只查 `admin`，拿到这把 token 的 agent 就能给自己调档，§8.9 的授权就成了 agent 自己能改的约定。
所以这类端点除了查 scope，还要求凭据是 `session`、主体是 `human`。这和 WebUI 添加 passkey 用的是同一个条件
（[`internal/authn/handler.go`](../../internal/authn/handler.go) 的注册入口）。这条进 [`TestACL`](../../internal/api/api_test.go)：
对这类路由，持有全部 scope 的 token 也必须拿到 403；放宽类路由，没提权的人的会话也必须拿到 403。
收紧也走这条：允许 agent 吊销别人的凭据没有用处，多一种例外只会多一条要测的路径。

这一节不新增任何绕过授权的路径。界面只是 `identity_grants` 的又一个写入方，经的是 CLI 同一组函数：
硬上限照样在写授权行的 [`setGrant`](../../internal/store/grants.go) 和 `Decide` 两处成立（§13.5），提交时的判定一处不变。

**审计也因此更准。** 从 WebUI 来的管理动作记成 `human:<operator>`，带具体的 `credential_id`；
CLI 只能记 `system` 加 `host_user`（§13.7）。信任根之外的动作值得进界面，这是另一个理由。

**M3 的落法**：端点在 [`internal/api/api.go`](../../internal/api/api.go) 的 `routes`，处理在 [`settings.go`](../../internal/api/settings.go)；
收紧的判定在 [`store/tighten.go`](../../internal/store/tighten.go) 和 [`sendpolicy/tighten.go`](../../internal/sendpolicy/tighten.go)。实现时定下的几处：

- **「只认人的会话」写在路由表里**：权限类路由标 `authority()`，OpenAPI 里只列 `session` 一种认证；
  处理函数之前统一查 [`Viewer.HumanSession`](../../internal/auth/auth.go)，添加 passkey 用的是同一个函数。
  总是放宽的路由（签发凭据、加身份、整行写授权）标 `widening()`，处理函数之前再查 `Viewer.Elevated`；
  可能放宽也可能收紧的（删授权、停用身份）先按收紧试，放宽时才看 `Viewer.Elevated`。
  提权本身是 `/auth/passkey/elevate/begin` 和 `finish`，只接受这个会话的人自己的 passkey、要求用户验证
- **放宽经 CLI 同一组函数**：签发走 [`authn.IssueToken`](../../internal/authn/authn.go)，记成人和会话而不是 `system`；
  主体「agent / service 首次用到时创建，人必须已存在」在 [`store.Grantee`](../../internal/store/identity.go)；
  白名单的格式在 [`sendpolicy.ParseAllowlist`](../../internal/sendpolicy/sendpolicy.go)。界面和 CLI 加身份时，出口都只能选库里启用着的
  （`transports`，§17）：`serve` 启动时、`identity add` 运行时都会为 `YUUKA_TRANSPORTS` 里的每个名字建一行占位，
  所以只在环境变量里的出口也选得到，打错的名字则不会变成一行
  跟着信走的决定（认领、取消认领、期望域）只看 scope：`prefix:manage` 加 `prefix:read`，
  因为回答里带着前缀现在的统计，也就是 `yuuka prefix expect` 打印的那份「对已收的信意味着什么」
- **收紧按生效值逐列比较**。两边都是生效值（[`sendpolicy.Applied`](../../internal/sendpolicy/tighten.go)：空限额换成按主体类型的默认数，
  档位过了 owner cap），每一列都不能比现在宽，否则整条拒绝、什么都不改，回 `403 widening_requires_cli`
  （名字早于提权；收紧端点仍然只收紧），`detail` 写出在主机上做这件事的 `yuuka grant set …` 命令。
  要放宽就整行写（`PUT /api/v1/admin/grants`），那条路由要求提权。从窄到宽：

  | 列 | 顺序 |
  |---|---|
  | `send_policy` | `none` < `draft` < `review` < `auto` |
  | `recipient_scope` | `thread` < `any`，`allowlist` < `any`；`thread` 和 `allowlist` 互不可比 |
  | `recipient_allowlist` | 新名单是旧名单的子集（大小写不敏感） |
  | 三项限额 | 数字越小越窄；不限（人的默认）比任何数字都宽 |
  | `hold_seconds` | 撤回窗口越长越窄（空值按默认的 600 秒算，`0` 是没有窗口） |
  | 暂停 | 暂停 < 不暂停 |

  取舍：
  - 降到 `draft` 时其余各列暂不起作用，仍然逐列比较：此刻存下的 `any`，会在以后有人把档位调回 `auto` 时
    一起生效，而那个人读到的是那时的授权，不知道这一列是什么时候变宽的。名单不论范围是不是 `allowlist` 都只能删条目，理由相同
  - 范围是偏序，不是一条线：`thread` 能回给一个对话里的任何通信方，包括以后才加进来的人；名单能写给还不在任何对话里的人。
    两者互不包含，所以 `thread ↔ allowlist` 两个方向都算放宽，要整行写、先提权
  - 撤回窗口（§8.9）在收紧里只能加长：窗口变短就是留给人撤回的时间变少，和调高限额一样是放宽。
    加长的代价只是 review 放行的信晚一点发出，误操作的后果和其他收紧同级，所以不必提权
- **删授权不一定是收紧**：删掉之后适用 §13.5 的默认，可能比这一行宽（agent 自己身份上写的 `draft`，删掉就成了默认的 `review`）。
  所以删除也要比较（[`Grant.RemovalWidenings`](../../internal/store/tighten.go)）：默认不发信（`none` / `draft`）时只比档位——
  默认没有存下任何一列，其余各列不会日后生效；默认会发信时逐列比，撤回窗口比的是真正起作用的那个：`review` 档是它的 `hold_seconds`，`auto` 档没有窗口（算 0）。
  已停用身份上的授权总能删：有没有这一行都发不出去。列表里每条授权带 `removal_widens`，界面据此在确认时说明会放宽哪几列，
  放宽的删除先提权
- **硬上限仍在写入的那一处**：CLI 和界面都经同一个 `setGrant` 写行。人的身份上给别人 `review` / `auto`，
  按生效值并不更宽（照样压成 `draft`），由 `setGrant` 以 `ErrOwnerCap` 拒绝，REST 回 `409 conflict`
- **停用身份也可能是放宽**：地址身份停用后，这个地址落到同 zone 的 `*@zone` 身份（§9.3），换了 owner、也换了一整套授权。
  所以 zone 里有启用的 pattern 时，停用地址身份要先提权（列表的 `taken_over_by` 指出是哪个），或在主机上 `yuuka identity rm`；
  停用 pattern 本身总是收紧
- **凭据只列 `token` 和 `smtp_password`**，和 `yuuka token ls` 是同一个查询，从不带秘密；会话不列，由登出结束。
  吊销和 CLI 走同一个 [`authn.RevokeToken`](../../internal/authn/authn.go)
- 界面在设置页的凭据、身份、授权三节，收紧怎么只给出更紧的值、放宽怎么接上提权见 §12.12
- 授权的每次写入（界面的收紧、整行写、删除，CLI 的 `grant set` / `rm`）都先取提交判定用的那把授权锁（`lockGrant`）再锁行：
  返回时，已经没有基于旧授权的判定还在进行，暂停在按下的那一刻之后就成立

#### 出口、渠道与机密的权限

> 状态：已实现（§17）。主机上的命令（`transport`、`secret`、`channel`）和 REST / WebUI（P6）都已落地：路由在
> [`internal/api/api.go`](../../internal/api/api.go) 的 `routes`，处理在 [`thirdparty.go`](../../internal/api/thirdparty.go)，
> 和 CLI 共用的部分在 [`internal/manage`](../../internal/manage/manage.go)；落地时定下的几处见 §17.6。

出口、身份的出口链、通知渠道和绑定进库之后（§17），它们也是「改变谁能做什么」的配置：
改了出口的主机，信和密码就交给了另一台服务器；多一条绑定，摘要和撤回按钮就多了一个去处。
所以按同一条线分：**任何机密写入都算放宽，改变信或摘要的去向也算放宽**；只会让系统更保守的是收紧。
全部要人的会话，不能只凭 `admin`，理由同上文（agent 拿到带 `admin` 的 token 不能给自己加一个出口）。

| 动作 | 在哪 | 会话 |
|---|---|---|
| KEK（或以后的 Vault / KMS）的配置与轮换、换包装器、`secret rewrap` / `check`、从环境变量导入 | 只在主机 | — |
| 新增出口；改它的主机、端口、用户名或机密；启用出口 | CLI 或 WebUI | 再验证 |
| 改出口的 caps（两个方向都会改变发出去的字节） | CLI 或 WebUI | 再验证 |
| 设置身份的出口链、加备用 | CLI 或 WebUI | 再验证 |
| 去掉备用、停用出口 | CLI 或 WebUI | 人的会话 |
| 新增或启用渠道、轮换 bot token、发起或确认绑定 | CLI 或 WebUI | 再验证 |
| 撤销绑定、停用渠道、订阅或退订自己绑定的事件 | CLI 或 WebUI | 人的会话 |
| 拉取账户：`account add`；`set` 改任何去向或给新密码；`resume`（M3.5） | CLI 或 WebUI | 再验证 |
| 拉取账户：改 `trusted_authserv_id`，包括清空（M3.5） | CLI 或 WebUI | 再验证 |
| 拉取账户：加文件夹、扩大回填、调高大小上限或日预算、缩短轮询间隔（M3.5） | CLI 或 WebUI | 再验证；只往小里改的是收紧，人的会话即可 |
| 拉取账户：`access` 给一个主体可读、`default_read` 改成可读、`--default` 而默认是可读（M3.5） | CLI 或 WebUI | 再验证 |
| 拉取账户：`access` 给一个主体不可读、默认改成不可读、`pause`、`rm`（M3.5） | CLI 或 WebUI | 人的会话 |
| 拉取账户：`test`、`sync`（M3.5） | CLI 或 WebUI | 人的会话；不改配置 |
| 连接 OAuth（M3.5b） | CLI 或 WebUI | 再验证；回调要会话加 state cookie 和 PKCE |

- **订阅不算放宽**：目的地在绑定时已经确认过，`mail.new` 的摘要又按订阅者的可见性过滤（§13.8），
  订阅一类事件不会让任何人看到他原本看不到的东西
- **改 caps 两个方向都是放宽**：放宽的 caps 让 yuuka 自己写 `Message-ID` 一类的头，收紧的会让已入队的字节在这个出口上发不出去（§10.2.2）。
  两者都改变发出去的东西，不是「更保守」
- **去掉备用是收紧**：信只是少了一条路，最坏是等主出口恢复
- **拉取账户的几处**（M3.5）：改来源的去向或密码，信从哪进来、密码交给谁就变了，所以是放宽；
  改 `trusted_authserv_id` 决定哪条头被当成事实，进而决定泄漏、仿冒判定和降级，两个方向都算放宽——清空看似收紧，
  却让可信接收方报了 `dmarc=fail` 的信不再降级（§10.1.1）。多拉进来的信对能读这个账户的所有人和 agent 可见，
  所以加文件夹、扩大回填这类也是放宽，只往小里改的才是收紧。`test` 把密码交给它本来的去处，不记审计，和 `transport test` 一致。
  每次放宽写 `actions`（`account.add`、`account.set`、`account.access`、`account.resume`），参数含名字、地址、主机、端口、用户名、
  authserv-id、audience、包装器和钥匙引用，从不含密码或它的哈希。REST 和 CLI 共用 `internal/manage` 里的同一组检查
- **拉取本身不推告警**：只有配置改动推 `security`，和出口一样；登录被拒在 `account ls` 和界面里显示，日志每天报一次（§10.1.3）
- 每次放宽写 `actions`、推 `security` 告警（§8.4）；参数从不含明文，也不含明文的哈希（§17.6）

**谁是管理员**（`security` 告警的收件人，§8.4）：**一个启用中（`disabled_at` 为空）的 human 主体，它用 passkey 登录得到的会话带 `admin`**。
这是一个主体上的属性，在发告警的那一刻按 `principals` 行算出来，不看此刻有没有人登录着。考虑过、没有选的两种：

- 「此刻持有一把未吊销、带 `admin` 的凭据」：human 只有会话这一种凭据（§13.3），会话 7 天绝对过期、登出即撤销。
  按这个定义，运维者一登出就不再是管理员，告警恰好在他不在场的时候不发——那正是攻击者最想要的窗口
- 「曾经被授予过 `admin`」：库里没有这条历史，也收不回来；停用一个人之后他仍是收件人

今天会话的 scope 是固定的全部（`auth.AllScopes`，§13.4），所以每个启用中的 human 都是管理员，「非管理员的人」这个集合是空的。
它变得非空的条件是给 human 的会话按主体限定 scope（比如只能读信的家人）；那时管理员就是会话 scope 含 `admin` 的那些人，
定义不变，这里也不用改。代价：在那之前多 human 的部署里所有人都收全部告警
- 路由照上文标 `authority()`；要再验证的标另一种（实现里叫 `widening()`），`TestACL` 对两种都断言 token 拿到 403
