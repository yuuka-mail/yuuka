# 第三方机密与通知渠道

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 17. 第三方机密与渠道：进库加密，信任根留在主机

> 状态：已定，未实现。迁移是 0017（机密、出口、身份的出口链）和 0018（通知渠道），
> 表的形状在 §4 的设计块里；WebUI 上的放宽动作依赖 §13.10 的当场再验证（0015）。分期见 §17.9。
> 实现之前，出口和 Telegram 仍按 §10.2、§8.4 写的现状从环境变量读。

M2 把出口当作部署事实：按名字定义在环境变量里，`identities.transport` 只存名字（§4 的原注）；
Telegram 也是一组环境变量，一个 bot、一个 chat、一个人。当时写下的回头条件是「出现需要在运行时增删出口的场景」。
现在出现了：多个自有域、多个外部邮箱（M3.5），每个域或邮箱有自己的出口和凭据，一个身份该走哪条出口取决于它所在域的 DNS；
通知也不止一个渠道、不止一个人。照旧做下去，每加一个域就要改环境变量、重启，
凭据和用它的身份、账户分在两处，改了一处另一处不知道。

所以：**第三方凭据**（出口的密码或 token、bot token、M3.5 的 IMAP 密码和 OAuth token）
连同它们描述的去向一起进库，机密部分加密；决定「信从哪进来、谁能解密这些密文、邮件内容能不能离开部署」的几样留在主机。

**为什么单独成节，而不放进 §13。** §13 的凭据（`credentials`）是别人拿来向 yuuka 证明身份的，只需校验，全部哈希；
这里的是 yuuka 拿去向第三方证明身份的，必须能还原成明文。持有者、威胁和生命周期都不同，
混在一张表里正是 §13.3 原来那句「`oauth` 类放 `credentials.secret_enc`」的问题（§17.4）。
这件事横跨 §4（表）、§8.4（渠道）、§9.3（身份的出口链）、§10.2（出口与执行器）、§13.7 / §13.10（认证与权限），
那几节各写各的部分；这里写它们共用的：边界、加密、上锁、升级、分期。

### 17.1 进库的，和留在主机的

进库：

- **出口**：类型、主机、端口、认证方式、用户名、caps、密码（Cloudflare Email Sending 的是 API token，当 SMTP 密码用）；
  每个身份的出口链（§9.3、§10.2.2）
- **通知渠道**（现在只有 Telegram bot）、**渠道绑定**（哪个 chat 属于哪个 human、谁按的按钮算数）、**事件订阅**（§8.4）
- M3.5：拉取账户的来源（IMAP 主机、用户名、密码或 OAuth refresh token、OAuth client secret）和每个拉取账户的
  `trusted_authserv_id`（§17.8）

Cloudflare API token 只有 yuuka 真要调 Cloudflare API 时才需要，到那时加一种机密类型，不另建表。

留在主机的是**信任根**：

| 项 | 为什么不进库 |
|---|---|
| 数据库连接串 | 连上库之前就要用 |
| KEK 文件（新增 `YUUKA_KEK_FILE`），或配置了 Vault / KMS 时它的地址和访问凭据 | 密文的价值全在包装 DEK 的那把钥匙（§17.2）。和密文放在同一处，加密就只防得住「拿走了备份、没拿走主机」；Vault / KMS 的访问凭据也不能放进它保护的库里 |
| R2 的两把令牌 | 收信必须在没有 KEK、包装器连不上、机密子系统整个坏掉时照常工作（§3.5 不丢信）。所有自有域共用同一对 bucket，加一个域不需要新的 R2 配置；两把令牌的权限划分本身在执行「Go 只写 `sent/`」（§2.2） |
| `YUUKA_WEBHOOK_SECRETS` | 理由同上：webhook 是收信的快路径，不能依赖包装器；轮换要和 Worker 的部署配合，本来就是主机上的事 |
| `YUUKA_TRIAGE_*`（含 token 文件） | 决定邮件内容会不会离开部署（§15）。这是数据出境的决定，属于主机，不属于一次可能被劫持的会话 |
| 提交口 TLS 文件、WebAuthn origin / RP ID、可信代理、`YUUKA_MCP_TOKEN` | 主机或客户端自己的事实，不是第三方凭据 |

`YUUKA_OUTBOX_WORKERS` 这类进程调参也留在环境变量里：它们不是凭据，也不随域、账户增减。

第二个 Cloudflare 账户下的域要第二套 Worker、bucket 和 webhook 密钥，不在本节范围；真要做，仍放主机配置，理由同 webhook。

### 17.2 静态加密：信封加密

**每条机密一把自己的数据钥匙（DEK），DEK 再由一个可替换的包装器包起来。**

- **载荷在本地封**：写入一条机密时生成一把随机的 DEK（每次写入换一把新的），用 AES-256-GCM 封住明文。
  AAD 各字段带长度前缀拼接（理由见 §13.3），含固定的用途串、格式版本、机密 id、类型和 **audience**。
  载荷开头有格式版本，换算法时按版本区分新旧，不用猜
- **DEK 由包装器包**：库里每条机密存「包装器 id + 钥匙引用 + 包好的 DEK」，和封好的载荷并列。
  包装时把机密 id 作为包装器的上下文（本地是 AAD，Vault Transit 是 context 或 associated data，云 KMS 是 encryption context），
  包好的 DEK 不能挪到另一行用
- **今天只有一种包装器：`local`**。钥匙是主机上的 KEK（`YUUKA_KEK_FILE`），钥匙引用是 KEK 的指纹（`kek_id`：
  从 KEK 派生、认得出是哪一把、又不泄露它），包装也是标准库的 AES-256-GCM，密钥由 KEK 经 HKDF 派生
- **以后可以换成 Vault Transit 或云 KMS**：它们的形状正好是「按钥匙引用加密、解密、重新包装一小段数据，钥匙有版本，带上下文」。
  存储格式已经带着包装器 id 和钥匙引用，所以换过去不需要数据迁移：新写入的机密用新包装器，
  旧的逐行把 DEK 解开再用新包装器包上（`yuuka secret rewrap`），载荷一个字节不动
- **Vault / KMS 只见过 DEK，从没见过机密**。它们的审计日志里没有第三方密码；单拿到 Vault 的权限、没有库，也得不到任何一条机密
- 全部用标准库（`crypto/hkdf`、`crypto/aes`、`crypto/cipher`）。每把 DEK 只封一次，随机 nonce 的上限无从谈起；
  本地 KEK 包装的次数十年也只在 10^6 量级，离上限很远。所以不用 `x/crypto` 的 XChaCha20-Poly1305，少一个直接依赖
- **测试向量**：固定的 DEK、KEK、nonce 和字段放在 `testdata/secret/`，格式由向量钉住。字节级的布局以
  `internal/secret` 和这组向量为准，这里不写死

**代价**：每条机密多存一个包好的 DEK（几十到几百字节），每次解密多一次解包。本地包装器的解包是一次内存里的 AES；
Vault 或 KMS 是一次网络往返，但解密只发生在构建出口客户端、启动渠道轮询者时，按 revision 缓存（§17.5），不是每封信一次。

**为什么现在就做 DEK，而不是等到真要接 Vault 时再改。** 单层格式（KEK 直接封载荷）以后要换成信封，
得把每一行重新加密，而且那时旧 KEK 和新系统必须同时可用、同时信得过：这是一次全库的、不能半途而废的迁移，
还正好落在换密钥系统这种本来就紧张的时刻。现在多存一个字段，以后换包装器就只是 rewrap。

**audience 绑定留在本地，从不交给 Vault / KMS。** audience 是这份明文可以交给谁：
出口是 `smtps://<用户名>@<主机>:<端口>`，bot 是 `telegram:bot<数字 id>`，IMAP 是 `imaps://…`，
OAuth 是 `oauth:<token 端点>#<client id>`。它在载荷的 AAD 里，用的时候 Go 从拥有它的那一行**重算** audience，
和封进去的不一致就失败即关闭（「主机名在加密之后变了，请重新输入密码」）。
不交给包装器，是因为检查必须发生在明文被使用的地方，而且不能随包装器而变：各家的上下文语义不同，
包装器只碰 DEK，它也不知道一行出口的主机是什么。所以无论用哪种包装器，这条保证都一样。

audience 是防「能写库的人」的主要手段。要防的是拿到了 app 角色连接串、或者能改库的备份、但没拿到主机文件
（或 Vault / KMS 凭据）的人。没有 audience，他把 `transports.host` 改成自己的服务器，
yuuka 下一次发信就会在 `AUTH PLAIN` 里把 token 交过去。只把密文绑在行上挡不住：行还是那一行，变的只是它的一列。
所以改主机、端口、用户名一律要重新输入并重新封装，而这要包装器，只有带再验证的会话或主机上的 CLI 走得到（§17.6）。
同理，`secrets` 是一张通用表、各表一对一引用它：把一个出口的 `secret_id` 指到 bot token 那一行，
类型和 audience 都对不上，解不出来。

**明文只在进程内存里。** 机密的值是一个专门的类型（`internal/secret.Value`），在日志、字符串化、JSON 序列化时都打码。
任何接口都没有读回明文的路径，CLI 也不打印：只能设置、轮换、删除。搬主机就是库和包装器的钥匙一起搬。

### 17.3 包装器：缺失、错误、轮换、备份

本地包装器的 KEK 是一个文件，一行，32 个随机字节的 base64（`openssl rand -base64 32`）：能存进密码管理器，也能抄在纸上。
作为 Compose secret 挂载（属主是容器里的用户、只读），**从不作为环境变量的值**，也不放在备份卷上。
文件而不是变量的理由和出口密码一样（§10.2）：环境会出现在 `docker inspect`、`/proc/<pid>/environ` 和配置转储里。
`serve` 和写机密的命令（`transport add` / `set`、`channel add`、`secret …`）读它；其余命令不需要。
以后配了 Vault 或 KMS，它的地址和访问凭据取代 KEK 文件成为信任根，同样留在主机（§17.1）。

**包装器不可用时一律失败即关闭，但收信不受影响：**

| 情况 | 行为 |
|---|---|
| 包装器不可用：没设 `YUUKA_KEK_FILE`；以后是 Vault / KMS 连不上或拒绝 | `serve` 照常启动：收信、reconcile、WebUI、REST、MCP、提交口都正常，入队也正常（入队只要 caps，caps 不是机密）。需要机密的出口**上锁**：信在队列里等，`outbox.error` 写明原因，永不判 `failed` 或 `dead`；渠道不运行；启动时和之后每 10 分钟报一次 ERROR，WebUI 顶上有横幅；写机密的请求回 503。远端包装器恢复后，下一次领取自动解锁 |
| KEK 文件格式不对 | 拒绝启动：一个读不懂的 KEK 文件是配置错误，不是可以降级运行的状态 |
| 某一行的包装器或钥匙引用对不上任何已配置的钥匙 | 只锁这一条，日志写出包装器和钥匙引用 |
| 钥匙对得上，解包或解封失败 | 视为篡改或损坏：锁住，并经仍能用的渠道推一条 `security` 告警（§8.4） |

包装器不可用时渠道本身也上了锁（bot token 是机密），所以这种情况只能靠日志和 WebUI 横幅被发现。

**轮换只重新包装 DEK，在主机上做。** 本地包装器：新 KEK 放进 `YUUKA_KEK_FILE`，旧的列进 `YUUKA_KEK_PREVIOUS_FILES`（只用来解包），重启；
`yuuka secret rewrap` 逐行把 DEK 解开、用新 KEK 包上，载荷不动；`yuuka secret ls` 看每一行的钥匙引用，没有行再用旧钥匙时删掉旧文件。
Vault Transit 的钥匙自带版本，rewrap 用它自己的重新包装接口；KMS 同理。换包装器（`local` → Vault）也是同一个命令。
**KEK（或包装器的权限）和库的转储一起泄露时，该换的是第三方凭据本身**：对方手里已经有明文，rewrap 收不回来。

**备份**：`pg_dump` 里是封好的载荷、包好的 DEK 和元数据（主机、用户名、chat id），按敏感数据保管（§7）。KEK 单独离线备份。
KEK 丢了（或 Vault 里的钥匙被删）要重新输入所有第三方机密，但不丢信：原件、投递、决定都不依赖它。

### 17.4 表的形状为什么是这样

列和约束在 §4 的设计块里，这里只写取舍。

- **一张通用 `secrets` 表**，出口、渠道、以后的拉取来源各自用一个唯一外键引用它，而不是每张表各放一列密文。
  封装、解封、重新包装、`secret ls` / `check` / `rewrap` 都只有一条路径，包装器和钥匙引用集中在一处
- **`credentials.oauth` / `secret_enc` 保持不用。** 那张表放的是登录 yuuka 用的凭据（§13.3）；
  外部账户的出站 token 属于那个账户和它的来源，不属于任何主体。§13.3 原来让 M365 的 refresh token 住进 `credentials`，
  会让「吊销一个主体的凭据」和「断开一个外部账户」纠缠在一起。这两个值不删：删 CHECK 里的枚举值要迁移，留着不写没有代价
- **出口名一经创建永不改。** `send_attempts.transport` 记的是名字，那是发送事实（§4.2），改名会让历史指向别处。
  删除出口 = 停用并抹掉它的机密，行留着，历史照样解析得出
- **占位行**：0017 为每个出现过的 `identities.transport` 名字建一行出口，`configured_at` 为空，不能发信；
  然后建 `identity_transports`、删掉 `identities.transport` 这一列（同一个提交改掉所有读者）。
  迁移拿不到环境变量，所以只能先占位，等启动时的导入（§17.7）或人用 CLI 把它填上
- **caps 必填、没有默认值**，理由同 §10.2：它决定从每封信里剥掉哪些头，默认错了会悄悄改掉别人的信
- **一个渠道就是一个 bot**。Telegram 的 `getUpdates` 每个 bot 只允许一个轮询者（否则 409），
  所以「启用中的渠道里 `(类型, bot id)` 唯一」由部分唯一索引守住，不靠约定
- **绑定是可撤销的独立行**，带 chat id 和「谁按的按钮算数」的 Telegram 用户 id。一个 bot 下的多个 chat、多个人靠绑定区分。
  绑定只属于 human：推送只发给人（§13.8），这条在 Go 里查
- **绑定码只存哈希**，并记下 Telegram 在 `/start <码>` 时报来的 chat、用户和名字，供确认时展示（§13.7）

### 17.5 运行时：不热加载，每次现查

**不做重载信号，也不用 LISTEN/NOTIFY。** 执行器每次领取时联表取出这封信的出口链、每个出口和它的机密的 `revision`；
构建好的 SMTP 客户端按（出口 id，出口 revision，机密 revision）缓存，这是唯一的缓存。
暂停按出口 id 记，出口或机密换了 revision 就清掉暂停：修好密码，下一次领取就重试，不用重启，也不等暂停期满。
主机上用 CLI 改了，下一次领取就生效。LISTEN/NOTIFY 以后可以作为降延迟的优化加上，不是正确性的一部分。

**渠道**：一个管理协程每 15 秒读一次渠道表，每个启用的 Telegram 渠道跑一个轮询者，revision 变了就重启它。
分发器对每个事件现查绑定和订阅，不缓存，所以撤销绑定立即生效。15 秒是加渠道、换 token 的最长生效时间，
渠道变动以天计，这点延迟不值得一套通知机制。

### 17.6 谁能改，怎么改

权限表在 §13.10。要点：

- 全部要人的会话，不能只凭 `admin`；**任何机密写入都算放宽**，改变信或摘要的去向（主机、出口链、新渠道、新绑定、新来源）也算。
  放宽在 WebUI 上要当场再验证。去掉备用、停用出口、撤销绑定这类收紧只要人的会话；
  订阅和退订也是：目的地已经批准过，摘要按订阅者的可见性过滤（§8.4）
- **agent 排除在外**：没有 MCP 工具；REST 要人的会话，agent 进不来，`TestACL` 断言持有全部 scope 的 token 在每条新路由上都是 403。
  agent 只在提交结果里看到出口的**名字**（§8.9）
- **审计**：每次放宽写一条 `actions`（`transport.add`、`identity.route`、`channel.bind.confirm`、`secret.rewrap`、`config.import`……），
  参数含 id、名字、主机、端口、用户名、caps、audience、包装器和钥匙引用，**从不含明文，也不含明文的哈希或指纹**：
  人选的密码，哈希能被暴力破解。同时推一条 `security` 告警，发给管理员和当事人（§8.4、§13.10）。CLI 照旧记 `system` 加 `host_user`（§13.7）
- **WebUI**：密码框只写不读，留空等于不改，从不回显；机密的状态只显示「已设 / 加密时间 / 正常 / 上锁」

**命令**（名字是单向门，已定；用法随实现写进运维文档）：

| 命令 | 为什么存在 |
|---|---|
| `yuuka transport add` / `set` / `ls` / `rm` / `test` | 出口的增改查删；`rm` 是停用并抹掉机密（§17.4）；`test` 连接、`EHLO`、`AUTH`，不发 `MAIL`，换密码之后不必拿一封真信试 |
| `yuuka identity set <地址> --transports 主[,备…]` | 设身份的出口链；已有的 `identity add --transport NAME` 继续可用，等于只有一个成员的链 |
| `yuuka channel add` / `ls` / `rm` / `test` | 渠道的增查删和一条测试消息 |
| `yuuka channel bind` / `unbind` | 发起绑定（打印链接、等待对方 `/start`、在终端上确认）和撤销绑定 |
| `yuuka channel subscribe` / `unsubscribe` | 给一个绑定订阅、退订事件 |
| `yuuka secret ls` / `check` / `rewrap` | 列出每条机密的类型、audience、包装器、钥匙引用、加密时间；`check` 逐条解密并报告结果，不打印任何机密；`rewrap` 重新包装 DEK，用于 KEK 轮换和换包装器 |

机密**永不作为参数值传递**，只能是文件路径或 `-`（标准输入）：参数会出现在 shell 历史和 `ps` 里。

### 17.7 从环境变量升级：一次导入，一个版本的弃用期

**做法**：启动时一次性导入，环境变量里的出口和 Telegram 配置**导入之后弃用**，下一个版本删掉解析和导入代码。
被弃用的是 `YUUKA_TRANSPORTS`、`YUUKA_TRANSPORT_<NAME>_*`、`YUUKA_TELEGRAM_BOT_TOKEN`、`YUUKA_TELEGRAM_CHAT_ID`、
`YUUKA_TELEGRAM_OPERATOR_ID`、`YUUKA_TELEGRAM_OPERATOR`。

没有选「环境变量覆盖数据库」：那是两个事实来源，每个读者都要合并两边，弃用期过了也删不干净。

- **导入**：包装器可用（设了 KEK）、而环境变量里的某个出口在库里是占位或不存在时，`serve` 把它写进库（主机、端口、caps，密码从 `_PASSWORD_FILE` 读出后封装）。
  Telegram 的四个变量变成一个名为 `telegram` 的渠道，给 `YUUKA_TELEGRAM_OPERATOR` 指定的人（没设时给唯一的那个 human）建一条绑定，
  设了用户 id 就带按钮，并订阅今天已有的四类事件。这是唯一不经双向确认就建的绑定：环境变量本身就是主机上的确认
- 导入以 `system` 记审计（`config.import`，`via: env`），幂等；几个进程同时启动时靠条件 upsert 保证只写一次。
  库里已经配置好的出口或渠道，环境变量被忽略并报 WARN
- **没设 KEK**：环境变量里的出口和 Telegram 照旧在内存里工作，报 WARN。升级永远不会让发信停摆。
  这期间启动时仍为环境变量里每个出口名 upsert 一行占位（占位不含机密，不需要 KEK），
  这样 `identity add --transport` 引用一个只在环境变量里的出口时有行可指；`identity add` 本身只接受库里已有的出口名，打错的名字不会变成占位
- 升级的具体步骤（生成 KEK、挂载、核实、删掉旧变量）随实现写进运维文档

### 17.8 M3.5 的形状

现在画出来，是为了保证上面的表接得上；迁移随 IMAP driver 一起。

- `accounts.kind` 扩为 `cf_catchall` / `imap` / `smtp_only`，加 sync / writeback / offload 策略（§9.2、§9.6）
- `account_sources`：IMAP 主机、端口、用户名、认证方式、机密、OAuth client；`oauth_clients` 放 client id 和 client secret（机密）
- 出口加认证方式 `xoauth2` 和 `account_id`：Gmail、工作邮箱的 SMTP 复用账户的 refresh token，不另存一份
- Microsoft 的 refresh token 每次使用都可能换新：由 `system` 重新加密写回，不逐次写审计
- 新的机密类型 `imap_password`、`oauth_refresh_token`、`oauth_client_secret` 随这次迁移加进 CHECK
- **每个拉取账户的 `trusted_authserv_id` 在库里**，改它算放宽（要再验证）：它决定哪一条 `Authentication-Results` 被当成事实（§10.1.1）。
  Cloudflare 账户的那一个仍是 `YUUKA_TRUSTED_AUTHSERV_ID`，和收信快路径一起留在主机

### 17.9 分期

| 步 | 内容 | 依赖 |
|---|---|---|
| P0 | 本节和相关章节的设计 | — |
| E | 当场再验证（0015，§13.10）先合入 | — |
| P1 | `internal/secret`：信封格式、`local` 包装器（读 KEK、钥匙引用、HKDF）、封装与解封、AAD 与 audience、打码类型、`testdata/secret/` 向量 | P0 |
| P2 | 0017 与存储函数（出口、链、机密的 CAS；从带身份的旧库迁移的测试）；API 里按名字校验出口的地方改读库 | P0、E |
| P3 | outbox：从库解析、按链领取、按最严 caps 去头、字节可发性、按 revision 缓存、按 id 暂停、上锁 | P1、P2 |
| P4 | CLI `transport` / `identity set` / `secret`、出口的导入、运维文档 | P1、P2 |
| P5 | 0018 与分发器、渠道管理协程、`/start` 绑定、`mail.new` 的可见性、Telegram 的导入、CLI `channel` | P1、E |
| P6 | REST 与 WebUI 的设置（出口、通知）、绑定确认页、上锁横幅、`TestACL`、截图 | P3–P5、E |
| P7 | 删掉环境变量里的出口和 Telegram（晚一个版本） | P4、P5 已发布 |
| M3.5 | §17.8 的表、IMAP driver | P1、P2；P2 之后可以先做密码认证的 IMAP |

P3、P4、P5 可以并行，`cmd/yuuka` 的接线按顺序合。

### 17.10 刻意不做

- 从多个 Cloudflare 账户收信；调用 Cloudflare API
- Vault Transit 和云 KMS 包装器：格式已经留好（§17.2），等有人要时再写，不需要数据迁移
- webhook、ntfy、发给自己的邮件这几种渠道。它们都能套进「类型 + settings + 机密」加不带按钮的绑定：
  webhook 是 POST JSON 加 HMAC 头（机密类型 `webhook_signing_key`，audience 是 URL）；
  发给自己的邮件要一个系统身份和防循环（§8.9）。等有人要再加
- 按绑定过滤账户（「这个 chat 只推工作邮箱」）；记录每次推送，以便撤回后在所有绑定上收掉按钮
- 按出口的配额或限速
- 多个所有者共用一条机密；任何导出机密的方式
- Telegram 上的「确认发送」按钮（§8.4）

---
