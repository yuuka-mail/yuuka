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
| `yuuka channel` | 数据库；`add`、`test` 还要 `YUUKA_KEK_FILE`，`bind` 有它时才能回复 chat、推安全告警 |
| `yuuka account` | 数据库；加密或解开密码的（`add`、带 `--password-file` 的 `set`、`test`）还要 `YUUKA_KEK_FILE`，有它时放宽的改动才推安全告警，`ls` 有它时才能报出密码 `ok` |
| `yuuka transport` / `secret` | 数据库；写入或解开密码的（`transport add` / `set` / `test`、`secret check` / `rewrap`）还要 `YUUKA_KEK_FILE`，轮换期间加 `YUUKA_KEK_PREVIOUS_FILES` |
| `yuuka triage status` / `report` | 数据库、`YUUKA_TRIAGE_*` |
| `yuuka triage rescore` | 数据库、R2、`YUUKA_DATA_DIR`、`YUUKA_TRIAGE_*` |
| `yuuka mcp --stdio` | 数据库、R2、`YUUKA_DATA_DIR`、`YUUKA_MCP_TOKEN`；和 `serve` 一样设了 `YUUKA_TRIAGE_*` 才显示评分 |

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
| `YUUKA_TRUSTED_AUTHSERV_ID` | `mx.cloudflare.net` | 只信这个接收方写的 `Authentication-Results`，其余的视为发件人伪造。只管 Cloudflare 账户的信：拉取的外部邮箱各有自己的值，配在账户上，不读这一项 |

## 通知

通知渠道（Telegram bot）、绑定和订阅存在库里，用 [`yuuka channel`](cli.md#通知渠道) 管理，不再读环境变量。
下面四个变量**已弃用**，只在这一个版本里还有效，下一个版本删掉：设了 `YUUKA_KEK_FILE` 时，`serve` 启动时把它们导入成一个叫
`telegram` 的渠道和一条绑定，之后忽略它们并报 WARN；没设 KEK 时它们照旧工作，同样报 WARN。升级步骤见
[第三方机密](secrets.md#telegram-从环境变量升级)。

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TELEGRAM_BOT_TOKEN` | 空 | 已弃用。新信推一行摘要到 Telegram。和下一项一起设或都不设 |
| `YUUKA_TELEGRAM_CHAT_ID` | 空 | |
| `YUUKA_TELEGRAM_OPERATOR_ID` | 空 | 运维者的 Telegram 数字用户 ID。设了它，`review` 档扣留中的信的卡片带「撤回」按钮，只认这个人在上面那个 chat 里按的。和下一项一起设或都不设，且 chat ID 必须是数字 |
| `YUUKA_TELEGRAM_OPERATOR` | 空 | `human:<name>`：Telegram 上的撤回以这个人记入审计（`via: telegram`，没有凭据），必须是已有的、没停用的人 |

下面几段对库里的渠道同样成立。配了 bot，`review` 档的信被扣留或被升级为草稿时都会推一张卡片，内容由系统从发件行渲染：
发信身份、收件人、提交者、主题、附件、正文开头三行（这几行会经过 Telegram 的服务器）。
卡片发不出去只记日志，撤回窗口照常计时，不会因此延长。

撤回按钮靠长轮询（`getUpdates`）收回调：yuuka 不开任何入站端点，也不需要 webhook 密钥。
同一个 bot 设过 webhook、或者另一个进程也在轮询它时，Telegram 会拒绝（409），日志里会说；
用 `deleteWebhook` 去掉 webhook，或者给 yuuka 单独一个 bot。卡片上没有任何能发信的按钮，
Telegram 账号被盗最多能把信撤回。

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

## 第三方机密的加密钥匙

库里的第三方机密（出口密码、bot token）用 KEK 加密（[§17.3](../design/secrets.md#173-包装器缺失错误轮换备份)）。
KEK 文件是一行 base64，32 个随机字节：`openssl rand -base64 32`。用文件、不用变量值，挂载时只读，不放在备份卷上。

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_KEK_FILE` | 空 | KEK 文件的路径。不设时其余功能照常，需要机密的出口和渠道上锁等待；设了但读不了或格式不对，拒绝启动 |
| `YUUKA_KEK_PREVIOUS_FILES` | 空 | 轮换后的旧 KEK 文件，逗号分隔，只用来解开还没重新包装的机密。要和 `YUUKA_KEK_FILE` 一起设，不能重复列出当前的那个 |

生成、挂载、从环境变量里的出口升级、换 KEK 的步骤：[第三方机密](secrets.md)。

## 发信

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_OUTBOX_WORKERS` | `2` | 同时进行的发送数，1–32 |
| `YUUKA_OUTBOX_POLL_INTERVAL` | `30s` | 多久查一次到期的信。`serve` 自己入队的信立即发，不等这个间隔 |

出口本身存在库里，用 [`yuuka transport`](cli.md#出口与第三方机密) 管理，不再写在环境变量里。

## 发信出口

**已弃用**：这一节的变量只在这一个版本里还有效，下一个版本删掉。设了 `YUUKA_KEK_FILE` 时，`serve` 启动时把它们导入库里
（库里只有占位或没有的出口才导入，已经配置好的以库为准，环境变量被忽略并在日志里报 WARN），之后就该删掉；
没设 KEK 时它们照旧从内存里工作，日志里报 WARN。步骤见[第三方机密](secrets.md#从环境变量升级)。

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TRANSPORTS` | 空 | 已弃用。出口的名字，逗号分隔，就是 `yuuka identity add --transport` 引用的名字。`serve` 启动时和 `identity add` 运行时，库里还没有的名字会各建一行只有名字的出口 |

每个出口一组变量（同样已弃用），`<NAME>` 是名字转成大写、`.` 和 `-` 换成 `_`（`work-mail` → `WORK_MAIL`）：

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

## 分诊评分器

给每封新信打分的决策模型（[§15](../design/triage.md)）。默认关闭：不设 `YUUKA_TRIAGE_URL`，
一切照旧，只是没有评分。v1 只观测：分数存下来、显示出来，不隐藏任何信，不改推送。
评分器是单独跑的服务（Laya、Kev 一类，说 System One 协议，`POST /v1/systemone`），yuuka 只是它的客户端。

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_TRIAGE_URL` | 空（关闭） | 评分器的基础 URL，如 `http://127.0.0.1:31490`，yuuka 在后面加 `/v1/systemone`。没有下一项时只接受回环和私网地址，连接时按实际连到的地址再查一遍，主机名解析到公网也会被拒 |
| `YUUKA_TRIAGE_HOSTED` | 空 | 设为 `1` 才允许第三方托管的评分器（如 Jev）。只接受 `https`，并且要有 token 文件。**见下文：信的内容会离开部署环境** |
| `YUUKA_TRIAGE_NAME` | 有 URL 时必填 | 评分器的名字，写进每一条评分，如 `laya-ml-1`（小写字母、数字、`.`、`-`，最多 32 个）。**换模型就换名字**：只有当前名字的评分算数，新名字下的信会被重新评分，旧评分留着。升级时问题集换了版本（§15.3）效果相同，不用改名字 |
| `YUUKA_TRIAGE_MODEL` | 空 | 请求里带的 `model`，如 Laya 的 `multilingual`。空就让后端自己选。实际用的模型以后端回报的为准，记在每条评分里 |
| `YUUKA_TRIAGE_TOKEN_FILE` | 空 | 放评分器 bearer token 的文件路径，只在启动时读。token 不写在环境变量里 |
| `YUUKA_TRIAGE_LOOKBACK` | `30d` | `serve` 补评多久以内的信。可以写天数（`30d`）或 Go 的时长。更早的信用 `yuuka triage rescore` 显式补 |

`serve` 在新信入库后立即把它排进评分队列，另外每十分钟扫一遍回溯期内还没有当前评分的信。
同时最多两个请求、每秒最多四个；只评逃过 §4.3 降级的信（投到已认领前缀、DMARC 没失败），
被降级的信本来就藏着，不发出去评。评分器连续失败时暂停调用、隔一阵试一次，恢复后由扫描补上；
它挂着不影响收信、推送和其他任何功能。日志里记消息 id 和 HTTP 状态，从不记信的内容。

评分器看到的是：认证结论那一行、是否投到已认领前缀、同域以前是否给这个前缀来过信、Reply-To 是否换了域、
有没有 List-Id、machine / human 分类、有没有验证码和登录链接（只说有没有，不给值），
加上 From、主题和正文的前若干千字（包在不可信内容的边界里）。笔记、标签、提案、草稿、同线程的其他信和附件都不给。

**托管评分器的隐私代价**：设了 `YUUKA_TRIAGE_HOSTED=1`，每封被评分的信的发件人、主题和正文都会发给那个第三方，
包括验证码和登录链接所在的正文。每条评分记下 `endpoint`（`local` 或 `hosted`），哪些信发出去过随时查得到；
关掉托管之后已经发出去的收不回来。不确定就用本地的评分器。

## MCP（stdio）

| 变量 | 默认 | 说明 |
|---|---|---|
| `YUUKA_MCP_TOKEN` | `yuuka mcp --stdio` 必填 | 这个进程以哪把 token 的身份运行。`serve` 不需要：agent 通常经 HTTPS 的 `/mcp` 用自己的 token |
