# 排障

按现象查。命令的完整用法见[管理命令](cli.md)；用 Docker Compose 时加 `docker compose run --rm yuuka` 前缀，
日志用 `docker compose logs yuuka` 看。

## 信没到

先分清是没进 Cloudflare，还是进了没到 yuuka。

- **发件方收到退信 `550 5.1.1`**：Cloudflare 的路由规则不认这个地址。新建的规则 30–60 秒后才生效；
  否则检查这个域的 catch-all 是否指向 Email Worker（[Cloudflare 侧](cloudflare.md#email-worker收信)）
- **发件方收到 `421 4.3.0`**：Worker 写 R2 失败或超时了。这是临时失败，发件方会重投，不会丢；持续出现就看 Worker 的日志
- **Cloudflare 收下了，WebUI 里没有**：Worker 已经把原件和投递标记写进 R2，只是 webhook 没调通。
  yuuka 每 `YUUKA_RECONCILE_INTERVAL`（默认 5 分钟）扫一遍标记，会自己补进来。想立刻补：

  ```bash
  yuuka reconcile
  ```

  信总是隔几分钟才到：webhook 一直没调通，全靠定期补。查 Worker 和 `YUUKA_WEBHOOK_SECRETS` 的密钥是否一致、
  `INGEST_URL` 能否从外面访问。一直补不进来：看 yuuka 的日志，多半是 R2 令牌失效
- **信在，但不在收件箱里**：发往未认领前缀的信只静默入库，不推送、不进聚合视图。`yuuka prefix ls --state discovered` 里找到它，
  是你的地址就 `yuuka prefix claim`
- **从备份恢复之后少了信**：备份时刻之后到达的信还在 R2 里，`yuuka reconcile --full` 把每个原件和库对一遍，补回来

## 外部邮箱的信没进来

```bash
yuuka account ls
```

- **STATE 是 `paused`**：有人 `yuuka account pause` 了它，`yuuka account resume` 恢复
- **STATE 是 `paused: login refused`**：见[下一节](#外部邮箱停了paused-login-refused)
- **SECRET 是 `locked`**：`serve` 没有 KEK，所有外部邮箱都不拉，Cloudflare 收信不受影响。远端还留着信，挂回 KEK 重启 `serve` 就接着拉
  （[第三方机密](secrets.md#挂载)）
- **文件夹一行是 `not synced yet`、LAST-ROUND 一直不动**：`serve` 没在跑，或者连不上服务器（看日志）。`yuuka account test` 从主机上试一次连接和登录
- **FAILED 不是 0**：这几封信一直取不下来，被跳过了，其余的照常。`yuuka account sync <名字> --retry-failed` 让 `serve` 再取一次
- **回填很慢**：按日预算走，Gmail 缺省每天 2GiB；碰到服务器的下载上限也会停到第二天。`--daily-budget` 可以调，但调过 Gmail 自己的上限没有用
- **信在，但有一封只有头部**：它比 `--max-message-size` 大，正文留在远端

## 外部邮箱停了（paused: login refused）

服务器拒绝了这个账户的密码。yuuka 不重试，直接暂停：用错的密码反复登录会让 Gmail 账户被临时锁住
（[为什么](../design/drivers.md#1013-imap-拉取的一轮提交顺序与不丢信)）。常见原因：

- app password 被撤销了，或者 Google 账户改了密码（改密码会让所有 app password 失效）
- 关掉了两步验证：app password 随之失效
- Workspace 管理员关掉了 app password，或者要求用安全密钥登录
- 换了用户名或服务器之后给错了密码

处理：在 Google 账户里生成一个新的 app password，然后

```bash
yuuka account set gmail --password-file -
yuuka account test gmail
```

给了新密码，暂停自动解除，不用 `resume`。密码没变、只是想再试一次（比如服务器当时出了故障）才用 `yuuka account resume gmail`，
它用同一个密码登录。`test` 失败时会带上服务器的原话，有时它说明了原因

## 信解析得不对

解析失败的信不会丢：原件保留，信被隔离。修了解析器（升级到新版本）之后重新解析：

```bash
yuuka reparse --since 2026-01-01
```

`reparse` 只重建从原件派生的内容（正文、线程、头部字段），不碰你的标记、改判和认领，可以放心重跑。
升级时如果新版本需要补字段，`serve` 启动后会自己在后台补，不用手动 reparse；信多时第一轮会跑一阵。

## 信发不出去

```bash
yuuka outbox ls
```

看状态和最后的错误：

- **一直 `queued`，日志里有 `transport paused`**：出口连不上或认证被拒（密码失效）。
  yuuka 暂停这个出口而不是把信判死，暂停从 1 分钟起翻倍、最长 30 分钟；出口链上有备用时，暂停期间的信走备用。
  `transport set <名字> --password-file -` 换了密码，下一封信就用新的、暂停立即解除，不用重启；
  还写在环境变量里的出口换密码要重启 `serve`
- **一直 `queued`，错误以 `no usable transport:` 开头**：发信地址的出口链上没有一个能用的出口，后面逐个写着原因，
  `serve` 的日志里同样报 ERROR（原因变了就报，之后每 10 分钟一次）。信不会因此失败，配好之后下一次领取就发，不用重启：
  - `not configured`：只有名字的占位。用 `transport set` 配好，或者 `identity set --transports` 换出口
  - `disabled`：出口被 `transport rm` 了。用 `identity set --transports` 换掉
  - `locked: no key to open its password (YUUKA_KEK_FILE)`：没设 KEK，见[第三方机密](secrets.md#挂载)。
    这期间 `serve` 每 10 分钟报一次 ERROR `transports locked`
  - `locked: the key its password was sealed under is not loaded`：密码是用另一把 KEK 加密的，
    比如换 KEK 之后过早去掉了 `YUUKA_KEK_PREVIOUS_FILES`。日志里的 `transports locked` 写着它要哪把钥匙（`key`，和 `secret ls` 的 KEY 列对得上）
  - `locked: its password failed authentication` 或 `was sealed for another destination`：库里的密文或出口的主机被绕过 yuuka 改过，
    日志里是 `transport locked: its secret is not what was sealed for it`。先查清是谁改了库，再 `transport set` 重新输入密码
- **一直 `queued`，错误说 `no usable transport of the chain allows the header fields`**：信入队之后，链上出口的 caps 收紧了，
  或者换上了更严的出口，存下的这份信头没有出口肯收。每 10 分钟再看一次；把能收的出口放回链上，或者放宽 caps
- **`failed`**：出口回了临时错误，按 5 分钟、15 分钟、45 分钟……退避重试，最长间隔 6 小时，大约一天半后放弃
- **`dead`**：重试用完或被永久拒收，不会再发
- **`partial`**：部分收件人收下了，其余的被拒或还在重试
- **`unknown`**：可能已经发出，yuuka 不会自己重发。确认之后用 `yuuka outbox resolve <id> --sent | --resend | --abandon`
  （[各选项](cli.md#发件队列)）

## 信停在草稿里

这是授权在起作用，不是故障：超出档位、收件人范围或限额的信都停在草稿等人确认。
提交的结果里写着原因。要调整就改授权：

```bash
yuuka grant ls --identity alerts@example.com
yuuka grant set --identity alerts@example.com --principal service:monitoring --max-per-day 500
```

以人的地址发信时，不是本人提交的一律停在草稿，这条改不了；让服务用它自己拥有的地址
（[授权](cli.md#发信地址与授权)）。

## 启动时报数据库版本不对

```
yuuka: database is at version 4, this binary needs 5: run `yuuka migrate` as the schema owner (YUUKA_DATABASE_OWNER_URL), then start again
```

升级了二进制但没迁移。以拥有表结构的角色跑一次 `yuuka migrate` 再启动。

反过来是 `database is at version 5, this binary only knows 4`：库被更新的版本迁移过，而你回退了版本。
旧版本不认识新表结构，回退要连同升级前的备份一起恢复。

## 启动时报提交口配置不全

`YUUKA_SUBMISSION_LISTEN` 设了，却既没有证书文件也没有 `YUUKA_SUBMISSION_PROXIES`：yuuka 不肯在没有 TLS 的情况下接受明文。
给证书，或者写上终结 TLS 的代理的地址（[SMTP 提交口](submission.md#tls-由谁终结)）。

## SMTP 客户端报错

- **连上就被断开**：经代理进来却没带 PROXY 头，或者带了 PROXY 头却不在 `YUUKA_SUBMISSION_PROXIES` 里。
  看日志里 yuuka 记下的来源地址，和名单对一下
- **`535 5.7.8`**：用户名或密码不对。用户名是凭据 id，不是主体名
- **`550 5.7.1`**：这个服务无权以信头 `From` 里的地址发信，回复里写着原因

各个回复码的含义见[接入 SMTP](../integrate/smtp.md#回复码)。

## passkey 丢了

还能登上服务器：`yuuka admin enroll`，用打印的链接注册新的 passkey。
登不上服务器：在 `<YUUKA_WEB_ORIGIN>/recover` 用一个离线恢复码换一次注册（[第一次登录](cli.md#第一次登录)）。

## 登录被限速、审计里全是同一个地址

yuuka 在反向代理后面，却没有把代理写进 `YUUKA_TRUSTED_PROXIES`，所有人看起来都是代理
（[配置](config.md#httpwebuirestmcp)）。

## 健康检查

`GET /healthz` 不需要认证：能在短超时内连上 Postgres 就是 200，否则 503。反向代理用它做健康检查。
