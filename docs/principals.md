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

```sql
principals(
  id, kind, name, display_name,
  owner_id → principals,      -- 服务凭据归属于哪个 human，构成可撤销链
  created_at, disabled_at
)
-- kind: human | agent | service | rule | system
```

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

一个主体可以有多把凭据，各自独立撤销：

```sql
credentials(
  id, principal_id → principals, kind, label,
  secret_hash, scopes text[],
  expires_at, last_used_at, revoked_at
)
-- kind: session | token | smtp_password | oauth
```

分离的好处很具体：给某台机器发一把只能发信的 SMTP 密码，换机器时撤销这一把，
不影响同一主体的其他凭据，也不用改 principal 本身。
`last_used_at` 让"哪把凭据其实早就没用了"看得见。

`secret_hash` 只适用于 `session / token / smtp_password`——这三类只需校验。
`oauth` 类（§9 里 M365 的 refresh token）必须**可还原**，不能哈希：单独一列 `secret_enc`，
用本机 KEK（文件或 KMS）加密，和 `accounts.auth_ref` 用同一套密钥管理。
`pg_dump` 因此含密文，备份按敏感数据保管（§7）。

### 13.4 scope 是唯一的权限语言

```
message:read        delivery:read       prefix:read
message:annotate    draft:create        submission:send
prefix:manage       account:manage      rule:manage      admin
```

每把凭据持有一个 scope 子集。§8.2 的能力分级在这里落地：

| 主体 | 典型 scope |
|---|---|
| human 浏览器会话 | 全部 |
| `agent:arisu` 的 MCP token | `message:read` `delivery:read` `prefix:read` `message:annotate` `draft:create` `submission:send` |
| `service:monitoring` 的 SMTP 密码 | `submission:send` |

scope 只回答「能不能做这类动作」，**用哪个身份、做到哪一步由 §13.5 的授权表回答**。
所以 agent 持有 `submission:send` 不等于能发信：没有 `identity_grants` 的身份它用不了，
以人的身份它最多到草稿。两层都是 API 层的事实，不是提示词的约定（§8.2）。
注意 `message:annotate` 的动作必须可撤销（§8.8 的 `undo_token`）。

### 13.5 identity ↔ principal 授权

§9.3 已经把 identity 硬绑到 transport。现在再加一层：谁可以用这个 identity 发信。

```sql
identity_grants(
  identity_id → identities, principal_id → principals,
  send_policy,                    -- none | draft | review | auto（§8.9）
  recipient_scope,                -- thread | allowlist | any
  recipient_allowlist text[],     -- 地址或域
  max_per_hour int, max_per_day int, max_new_recipients_per_day int,
  hold_seconds int,               -- review 档的撤回窗口
  suspended_at timestamptz        -- 熔断或人工暂停
)
```

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

这几个端点各自认证，不要混。TG 容易被当成纯输出而漏掉，但它上面有「确认发送」按钮，
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
  不能只信一个请求头；验签失败就拒绝，而不是回落成无认证

### 13.8 ACL 的粒度到账户为止

如果某些内容不该让某个主体看到（例如受合规约束的账户不该进 agent 的上下文，§9.4），
用**账户级**开关表达，不要下沉到单封信：

```sql
account_access(account_id → accounts, principal_id → principals, can_read bool)
```

**单封信级别的 ACL 一律不做。** 一旦允许"这封信某主体看不到"，
就得在每个查询、每个聚合、每次搜索上带 ACL 过滤，复杂度跳一个台阶，
而且很容易出现"搜索结果数对不上"这类难查的 bug。账户级粒度足够表达真实需求，
且天然和 §9.2 的 `sync_policy` 对齐。

#### 可见性只算一次，所有读路径共用

账户级粒度省掉的是「每封信一份 ACL」，省不掉「每条读路径都要过滤」。
跨账户模型（§9.1）下，漏洞不在 `messages` 本身，而在那些顺手聚合、顺手推送的地方。所以：

- **可见性在一处定义**：`visible_deliveries(principal)` = 该主体有 `can_read` 的账户上的投递；
  一封 message 可见，当且仅当它至少有一条可见投递。**任一可读投递即可读**内容，
  但该主体看到的 `deliveries` 只含可见的那几条，`raw` / `auth` 也只给可读那一路
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
| M1 | **`principals` / `credentials` / `identity_grants` 建表**，WebUI 内置 passkey 登录、走 session 凭据 |
| M2 | SMTP submission 口（跟发信能力一起做）；MCP token |
| M3.5 | `account_access`（跟多账户一起做） |

和 §9.5 同样的理由：**表从 M1 就建**，因为主体模型是改起来最贵的部分。
M0 不需要这一层——那时只有一个人和一个脚本。
