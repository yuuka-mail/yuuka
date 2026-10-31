# 配置参考

yuuka 只从环境变量读配置，没有配置文件。这一页列出全部变量；
[`cmd/yuuka`](../../cmd/yuuka/) 里的测试会核对代码读的每个变量都在这里出现。
用 Docker Compose 时变量写在 `yuuka.env`，其中几项由 `compose.yaml` 代填，
Compose 自己的 `.env`（镜像、密码、备份策略）见 [deploy/README.md](../../deploy/README.md)。

时长的写法是 Go 的格式：`30s`、`5m`、`720h`。

## 各命令要什么

| 命令 | 需要 |
|---|---|
| `yuuka serve` | 下面全部的必填项 |
| `yuuka migrate` | `YUUKA_DATABASE_OWNER_URL`（没有时用 `YUUKA_DATABASE_URL`） |
| `yuuka reconcile`、`yuuka reparse` | 数据库、R2、`YUUKA_DATA_DIR` |
| `yuuka admin enroll` | 数据库、`YUUKA_WEB_ORIGIN` |
| `yuuka prefix` / `token` / `outbox` / `identity` / `grant` | 只要数据库 |
| `yuuka mcp --stdio` | 数据库、R2、`YUUKA_DATA_DIR`、`YUUKA_MCP_TOKEN` |

## 数据库

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_DATABASE_URL` | 必填 | Postgres 连接串。除迁移以外的一切都以这个角色运行，它不该有 DDL 权限 |
| `YUUKA_DATABASE_OWNER_URL` | 空 | 拥有表结构的角色，只给 `yuuka migrate`。其他命令拿到它也会顺带迁移，那是给开发用的，部署时不要给 `serve` |

每个命令启动时核对库的版本，和二进制不一致就拒绝运行（[见排障](troubleshooting.md#启动时报数据库版本不对)）。
为什么要两个角色：[数据库角色](../design/data-model.md#4-数据模型postgres)。

## R2 与本地副本

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_R2_ENDPOINT` | 必填 | `https://<account id>.r2.cloudflarestorage.com` |
| `YUUKA_R2_RAW_BUCKET` | 必填 | raw bucket 的名字 |
| `YUUKA_R2_RAW_ACCESS_KEY_ID` | 必填 | 只限 raw bucket 的 Object Read & Write 令牌 |
| `YUUKA_R2_RAW_SECRET_ACCESS_KEY` | 必填 | |
| `YUUKA_R2_PENDING_BUCKET` | 必填 | pending bucket 的名字，必须和 raw 不同 |
| `YUUKA_R2_PENDING_ACCESS_KEY_ID` | 必填 | 只限 pending bucket 的 Object Read & Write 令牌 |
| `YUUKA_R2_PENDING_SECRET_ACCESS_KEY` | 必填 | |
| `YUUKA_DATA_DIR` | 必填 | 原件的本地副本放在这里 |

两把令牌怎么建：[Cloudflare 侧的配置](cloudflare.md#r2两个-bucket两把令牌)。

## 收信

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_WEBHOOK_SECRETS` | `serve` 必填 | Email Worker 的签名密钥，每个至少 32 字节；逗号分隔，换密钥期间写两个 |
| `YUUKA_RECONCILE_INTERVAL` | `5m` | 多久扫一次投递标记，补进 webhook 漏掉的信 |
| `YUUKA_ACCOUNT` | `cloudflare` | 这个 Cloudflare 账户在库里的名字，第一封信入库时建。去重按账户算，**上线后不要改**：改名等于开一个新账户 |
| `YUUKA_TRUSTED_AUTHSERV_ID` | `mx.cloudflare.net` | 只信这个接收方写的 `Authentication-Results`，其余的视为发件人伪造 |

## 通知

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TELEGRAM_BOT_TOKEN` | 空 | 新信推一行摘要到 Telegram。和下一项一起设或都不设 |
| `YUUKA_TELEGRAM_CHAT_ID` | 空 | |

## HTTP：WebUI、REST、MCP

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_LISTEN` | `127.0.0.1:31480` | HTTP 监听地址。webhook、WebUI、REST、MCP、`/healthz` 都在这一个口上 |
| `YUUKA_WEB_ORIGIN` | 空 | 浏览器看到的源，如 `https://mail.example.com`。不设就没有 WebUI、登录和 REST；MCP 不依赖它 |
| `YUUKA_WEBAUTHN_RP_ID` | 源的主机名 | passkey 绑定的域。**上线后改它会让所有已注册的 passkey 失效** |
| `YUUKA_TRUSTED_PROXIES` | 空 | 反向代理的网段，逗号分隔，如 `127.0.0.1/32,::1/128`。只有来自这些地址的 `X-Forwarded-For` 才被采信 |

不设 `YUUKA_TRUSTED_PROXIES` 时 yuuka 把 TCP 对端当成客户端。放在代理后面却不设，所有人看起来都是代理：
登录限速成了一个共享的桶，谁都能刷爆它把你挡在外面，审计也只记到代理。
代理前面还有 CDN 时，CDN 的网段也要写进来，否则 yuuka 会停在 CDN 节点上。
信任一个没人经过的网段只会让那里的人能自报地址，所以客户端直连代理时不要写 CDN 的网段。

## 发信出口

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TRANSPORTS` | 空 | 出口的名字，逗号分隔，就是 `yuuka identity add --transport` 引用的名字。不设就什么都不发，信留在队列里 |
| `YUUKA_OUTBOX_WORKERS` | `2` | 同时进行的发送数，1–32 |
| `YUUKA_OUTBOX_POLL_INTERVAL` | `30s` | 多久查一次到期的信。`serve` 自己入队的信立即发，不等这个间隔 |

每个出口一组变量，`<NAME>` 是名字转成大写、`.` 和 `-` 换成 `_`（`work-mail` → `WORK_MAIL`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TRANSPORT_<NAME>_HOST` | 必填 | SMTP 服务器。只用隐式 TLS |
| `YUUKA_TRANSPORT_<NAME>_PORT` | `465` | |
| `YUUKA_TRANSPORT_<NAME>_USERNAME` | 空 | AUTH PLAIN 的用户名 |
| `YUUKA_TRANSPORT_<NAME>_PASSWORD_FILE` | 有用户名时必填 | 放密码的文件路径。只在启动时读，换密码后重启 |
| `YUUKA_TRANSPORT_<NAME>_PASSWORD` | 不能设 | **密码从不写在环境变量里**：设了这一项 yuuka 拒绝启动，提示改用文件 |
| `YUUKA_TRANSPORT_<NAME>_CAPS` | 必填 | 出口允许 yuuka 自己定哪些头：`cloudflare`、`standard`（= `message-id,return-path`）、`none`，或 `message-id`、`return-path`、`dkim`、`status-query` 的组合 |

`CAPS` 写错的后果是信被出口拒收，或者 yuuka 以为能对账而其实不能。不确定就从 `none` 开始；
Cloudflare Email Sending 用 `cloudflare`。各个出口能做什么：[能力矩阵](../design/drivers.md#103-能力矩阵)。

例子：

```bash
YUUKA_TRANSPORTS=cloudflare
YUUKA_TRANSPORT_CLOUDFLARE_HOST=smtp.mx.cloudflare.net
YUUKA_TRANSPORT_CLOUDFLARE_USERNAME=...
YUUKA_TRANSPORT_CLOUDFLARE_PASSWORD_FILE=/run/secrets/cloudflare-smtp
YUUKA_TRANSPORT_CLOUDFLARE_CAPS=cloudflare
```

## SMTP 提交口

让同一部署环境里的其他服务用 SMTP 经 yuuka 发信。默认关闭。怎么选暴露范围和 TLS 的终结方式：[SMTP 提交口](submission.md)。

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_SUBMISSION_LISTEN` | 空（关闭） | 监听地址，如 `:465`。只做隐式 TLS，不做 STARTTLS |
| `YUUKA_SUBMISSION_TLS_CERT_FILE` | 空 | PEM 证书链。和下一项一起给时 yuuka 自己终结 TLS，文件变化后一分钟内换上新证书，不用重启 |
| `YUUKA_SUBMISSION_TLS_KEY_FILE` | 空 | PEM 私钥 |
| `YUUKA_SUBMISSION_PROXIES` | 空 | 提交口前面的代理的网段。它们必须发 PROXY protocol 头（v1 或 v2），也只有它们可以发。没有证书文件时必填 |
| `YUUKA_SUBMISSION_HOSTNAME` | 证书上的第一个 DNS 名，否则 WebUI 的主机名 | 问候语里报的名字。代理终结 TLS 时 yuuka 看不到证书，要写上 |

`YUUKA_SUBMISSION_PROXIES` 和 `YUUKA_TRUSTED_PROXIES` 是两份名单：同一个代理两边都用时把地址写两遍。
既没有证书文件也没有代理名单时 `serve` 拒绝启动。

## MCP（stdio）

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_MCP_TOKEN` | `yuuka mcp --stdio` 必填 | 这个进程以哪把 token 的身份运行。`serve` 不需要：agent 通常经 HTTPS 的 `/mcp` 用自己的 token |
