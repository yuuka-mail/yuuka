# 多主体：一个邮箱宇宙，多个主体

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

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
表定义见 [`0003_identity.sql`](../internal/store/migrations/0003_identity.sql)：`kind` 是 human / agent / service / rule / system 五种之一；
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
表定义见 [`0003_identity.sql`](../internal/store/migrations/0003_identity.sql)。

分离的好处很具体：给某台机器发一把只能发信的 SMTP 密码，换机器时撤销这一把，
不影响同一主体的其他凭据，也不用改 principal 本身。
`last_used_at` 让"哪把凭据其实早就没用了"看得见。

`secret_hash` 只适用于 `session / token / smtp_password`——这三类只需校验。
`oauth` 类（§9 里 M365 的 refresh token）必须**可还原**，不能哈希：单独一列 `secret_enc`，
用本机 KEK（文件或 KMS）加密，和 `accounts.auth_ref` 用同一套密钥管理。
`pg_dump` 因此含密文，备份按敏感数据保管（§7）。

可校验的三类秘密都由服务端生成，从不接受人选的值，所以 `secret_hash` 用 SHA-256 而不是慢哈希：
256 位的随机串没有可穷举的空间，慢哈希只会拖慢每个请求。交给持有者的字符串是
`<前缀>_<凭据 id>_<秘密>`，每类凭据一个前缀（格式见 [`internal/auth/secret.go`](../internal/auth/secret.go)）：
明文里带 id，按主键取行再做常量时间比较，而不是按哈希搜索；前缀让泄漏的串能被认出来，
也让一类凭据不会被当成另一类接受。

passkey 不是 `credentials` 的一行：它是产生 `session` 凭据的登录方式，本身不带 scope，
私钥也从不离开验证器。它单独一张 `passkeys` 表，列按 WebAuthn 的 credential record 展开
（公钥、签名计数、AAGUID、BE/BS 标志），不把任何库的序列化格式冻结进库里。

**`token` 的签发（M1）**：只有主机上的 CLI 能签发（`yuuka token issue` / `ls` / `revoke`，
参数见 [`cmd/yuuka/token.go`](../cmd/yuuka/token.go)），WebUI 入口还没有（§13.7 的 CLI 规则）。

- 只发给 `agent` 和 `service`。`human` 的凭据只有 passkey 登录产生的 `session`，
  `issue` 遇到 human 直接拒绝并指向 `admin enroll`：给人发一把长期 token，等于在 passkey 旁边开一扇不防钓鱼的后门。
  这条在 `authn.IssueToken` 里也查一遍，不只靠 CLI
- 指名的主体不存在就建（`display_name` 先等于 name），建主体本身记一条 `principal.create`
- token 明文只打印一次，单独一行写到 stdout（说明写 stderr），便于直接重定向进密钥文件；库里只有哈希，丢了只能吊销重发。
  `--ttl` 缺省为不过期：token 靠显式吊销收回，`ls` 的 `last_used_at` 让闲置的那把看得见
- `ls` 连已吊销的也列出，永远不显示秘密
- `revoke` 只收 `token` 类凭据（会话由登出撤销），记 `auth.credential.revoke`。
  吊销立即生效：REST 和 MCP 的 HTTP 形态每个请求重新认证，stdio 形态每次工具调用重新认证（§8.7），
  没有任何缓存的认证结果要等它过期

### 13.4 scope 是唯一的权限语言

scope 是一张封闭的扁平列表，定义在 [`internal/auth/auth.go`](../internal/auth/auth.go)，
库里 `credentials` 上的约束是同一份（[`0003_identity.sql`](../internal/store/migrations/0003_identity.sql)）：加一个 scope 要写迁移，这正是目的。

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
注意 `message:annotate` 的动作必须可撤销（§8.8 的 `undo_token`）。

### 13.5 identity ↔ principal 授权

§9.3 已经把 identity 硬绑到 transport。现在再加一层：谁可以用这个 identity 发信。

`identity_grants` 每行是一个（身份, 主体）对：档位 `send_policy`（§8.9）、收件人范围、各项限额、
`review` 档的撤回窗口和熔断时间。**表已建**（[`0003_identity.sql`](../internal/store/migrations/0003_identity.sql)），默认值取安全的一端；
**还没有任何代码读它**：档位、限额、硬上限的执行随发信（M2）落在 API 层。

各项限额的默认值由授权代码决定，不写在库里：库里的空值只表示「这条授权没有自己的数」，
用什么默认值、按主体类型怎么区分，是 API 层执行授权时的事，改默认值不需要迁移。

于是 `service:monitoring` 只能以 `alerts@example.com` 发，**无法冒用运维者本人的地址**。
服务凭据泄露的后果因此被限制在一个前缀内——这对 catch-all 尤其要紧，
因为域级 onboard 意味着"能发信"默认等于"能以任意前缀发信"（§9.3）。

档位和限额对所有主体是同一套机制，区别只在默认值：

| 主体 → 身份 | 默认档位 |
|---|---|
| `human:<operator>` → 自己的身份 | `auto`（人按发送就是确认） |
| `agent:arisu` → `arisu@example.com`（owner 是它自己） | `review` + `thread` |
| `agent:arisu` → 人的身份 | `draft`，**且不能调高**（§8.9 的硬上限） |
| `service:monitoring` → `alerts@example.com` | `auto` + `allowlist` |

服务主体也可以设成 `review` 或 `draft`：SMTP 客户端照常拿到 `250`，
信停在 `held` 或草稿里等审核 / 等人。这让「新接入的服务先观察一周」不需要另做机制。

### 13.6 SMTP submission：让其他服务直接发信

这是多主体带来的最大实用价值。yuuka 自己监听一个 **SMTP submission 口**
（`go-smtp` 的 server 侧，和 §5 选型里的 `go-imap` 同生态，不新增依赖），
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

**监听方式**（对 yuuka 所有的邮件协议监听器都成立，包括 M5 可能加的 IMAP 网关）。
实现在 [`internal/mailnet`](../internal/mailnet/mailnet.go)：它包住一个 TCP listener，
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
  常见的「一个 Caddy 同时做两件事」只是把同一个地址写两遍。配置项随提交服务一起加
- **头和 TLS 握手在 `Accept` 之前做完**，每个连接一个 goroutine，共用一个超时（默认 10 秒）。
  不在第一次 `Read` 时才懒解析：SMTP 是服务端先说话，懒解析意味着问候语在检查之前就发给了对方，
  `RemoteAddr` 也得变成会阻塞的调用。现在一个只发半个头就不动的对端只占自己的 goroutine 到超时，
  不耽误别人的 `Accept`；服务端看到的全是过了检查的连接。`Close` 会中断还在握手的连接
- **解析从严**：v1 最长 107 字节；v2 的长度字段上限 1 KiB（规范允许 64 KiB，但地址块最多 216 字节，
  剩下的 TLV 这里不读，直接跳过）；v2 的 AF_UNIX 和 UDP 来源拒绝，放在 TCP 邮件监听器前面没有意义，
  拒绝比猜一个地址好。解析器有 fuzz 测试

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
| SMTP submission | SASL → `smtp_password` 凭据 | 只有同一部署环境里的服务；默认不在公网地址上监听 |
| REST / MCP | Bearer → `token` 凭据 | 调用它的 agent 和服务 |
| TG 回调按钮（§8.4） | 校验 `callback_query.from.id` → 运维者的 `session` 凭据 | Telegram；改用长轮询则无需入站 |
| `GET /healthz` | **不认证**（唯一的例外） | 反向代理的健康检查 |

这几个端点各自认证，不要混。

**`/healthz` 为什么可以不认证**：反向代理的主动健康检查拿不出任何凭据，给它发一个 token
就得在代理配置里存一份长期凭据，比开放一个只答一位的端点更糟。它能开放，是因为它只回答
「进程在、能在短超时内 ping 通 Postgres」：200 或 503，正文是常量，不带版本、错误文本和耗时，
不查任何邮件数据、不写库、不按请求记日志；并发探测共用同一次 ping、约每秒最多一次，
刷它占不住连接池。代价是外人能知道这里跑着一个服务、它此刻是否连得上库——端口开着本来就暴露了前者，
后者对攻击没有增量。这几条约束一旦放松（比如想加版本号或依赖细节），它就不再是一个可以不认证的端点，
该改成带凭据的独立端点。路由清单与各自的认证方式在
[`cmd/yuuka/main.go`](../cmd/yuuka/main.go) 的 `routes`，清单测试保证新端点不会无意中漏掉认证。
WebUI 的页面壳（静态资源）同样不需要会话，但它不含数据，数据全走 `/api/`。TG 容易被当成纯输出而漏掉，但它上面有「确认发送」按钮，
就是 `submission:send` 的入口之一。把 REST / MCP 放在只认浏览器 cookie 的身份代理后面是个常见错误——
agent 不是浏览器，拿不到那个 cookie。

**WebUI 的内置登录**：

- 单一运维者，首选 **passkey（WebAuthn）**：没有密码可泄露，也不需要邮件找回——
  邮件系统自己的登录不该依赖收邮件
- 第一个 passkey 由 CLI 签发一次性注册链接（`yuuka admin enroll`），之后在 WebUI 里添加更多设备；
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
  由 WebUI 页面读出后提交给注册接口（路由见 [`internal/authn/handler.go`](../internal/authn/handler.go)）。令牌在 begin 时校验、
  在 finish 时与写入 passkey 同一事务里原子地消耗，所以并发的两次注册只有一次成功
- 会话：不透明随机串，库里只存哈希；cookie 用 `__Host-` 前缀、`HttpOnly`、`SameSite=Strict`，
  有绝对有效期、不滑动续期（再登录只是摸一下验证器）；登出即撤销那一行。
  cookie 属性、有效期和 `last_used_at` 的写入节流见 [`internal/authn`](../internal/authn/authn.go)
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
  遇到格式不对的项就退回对端地址，免得客户端靠发垃圾给自己换桶（[`internal/clientip`](../internal/clientip/clientip.go)）。
  这只在代理自己也按同样规则写这个头时成立：代理前面若还有 CDN，代理要把 CDN 的网段配成它的可信代理。
  恢复入口始终全局限速，被刷爆时运维者仍有 CLI
- 进行中的 WebAuthn 仪式（challenge）放在内存里，键是一个 `HttpOnly` cookie，用一次即删。
  重启只意味着重试一次；哪天要多副本部署再挪进 Postgres
- **主机上的 CLI 不登录，也不冒充任何人**。`yuuka admin enroll`、`yuuka prefix`、`yuuka token`
  在服务器上跑，能跑就是最高权限，但 yuuka 并不知道是哪个人在敲：它们写进 `actions` 的
  `actor` 是 `system`、`credential_id` 为空；管理命令（`prefix`、`token`）还在 `args` 里带 `via: "cli"`
  和执行它的系统用户名 `host_user`。
  记成 `human:<operator>` 会凭空制造一次没有发生过的登录；多人共用一台主机时，`host_user` 才是真实可查的那一条线索
- WebAuthn 的 RP ID 与 origin 来自配置（[`cmd/yuuka/config.go`](../cmd/yuuka/config.go)），
  不从请求里推断：origin 必须是 https，或 http://localhost

### 13.8 ACL 的粒度到账户为止

如果某些内容不该让某个主体看到（例如受合规约束的账户不该进 agent 的上下文，§9.4），
用**账户级**开关表达，不要下沉到单封信：`account_access` 每行是（账户, 主体, `can_read`）。
表已建（[`0003_identity.sql`](../internal/store/migrations/0003_identity.sql)），按它过滤读路径随多账户（M3.5）一起做。

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
  我们发出的信没有投递，按发信 identity 所属的账户算：Sent 副本所在 outbox 的 identity 的账户可读，这封信就可读（§4.6）
- **store 层强制**：每个读查询函数都要求一个 `Viewer` 参数，没有它就调不了。
  过滤靠类型系统保证存在，不靠每个调用点记得加 `WHERE`
- **所有读路径都过这一道**：

| 读路径 | 受限主体看到的 |
|---|---|
| 搜索 / `search_messages` | 结果和**计数、分面**都只在可见消息上算 |
| `get_message` / 附件 / 结构化视图 | 消息可见才可取；附件随消息 |
| `extractions` / `get_otp` | 只来自可见消息 |
| 线程 | `n`、`participants`、`first_at` / `last_at`、`snippet` 都按可见消息**现算**；`threads` 表里存的聚合值只服务不受限的视图 |
| `list_prefixes` / 统计 | 只在可见投递上聚合 |
| SSE / `changes` / JMAP `queryChanges` | 目标不可见的事件跳过，游标照常前进 |
| `actions` / `proposals` | 审计日志只给 human 会话；agent 只看自己发起的 |
| 推送通知 | 只发给 human，不经过受限主体 |

测试上用一个「含受限账户」的固定夹具，表驱动地把上表每条读路径跑一遍，断言没有泄漏。
新增读路径就是往这张表加一行。

**随 M3.5 再定的两件事**（M2 不过滤读路径，所以现在不必定）：

- 草稿和 outbox 的变更事件（`changes` 的 `draft`、`outbox` 两类）对谁可见：
  按发信身份所属账户、按提交者（`drafts.principal_id`、`outbox.submitted_by`），还是两者之一
- agent 没有某账户的 `account_access` 时，能不能看到它自己以该账户下的身份发出的信。
  §4.6 的规则目前只按账户算，答案是不能

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
| M2 | SMTP submission 口（跟发信能力一起做）；`accounts` / `identities` 建表（§9.5），`identity_grants`、`account_access` 补外键（已落地：[0005](../internal/store/migrations/0005_sending.sql)） |
| M3.5 | `account_access` 生效：读路径按 §13.8 过滤（跟多账户一起做） |

`identity_grants` 和 `account_access` 在 M1 只建表、不读：外键指向的 `identities`、`accounts`
当时还不存在，这两列是不带外键的文本。M2 的迁移建了这两张表并补上外键：`account_access` 里原有的账户名
换成账户 id；`identity_grants` 不可能有合法的行（没有 identity 可指），有行时迁移拒绝执行、
要人先清掉，而不是静默丢掉一条授权。

主体与凭据的表从 M1 就建，因为主体模型是改起来最贵的部分。
M0 不需要这一层——那时只有一个人和一个脚本。
