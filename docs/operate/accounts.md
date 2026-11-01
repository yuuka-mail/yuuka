# 外部邮箱

yuuka 用 IMAP 把外部邮箱拉进来，和 catch-all 收到的信放在同一个收件箱里。现在只支持密码登录，实际能用的是 Gmail 的 app password；
OAuth（Gmail OAuth、Microsoft 365）还没有。命令的参数见[管理命令](cli.md#外部邮箱)，这里讲第一次接上一个邮箱的步骤和几处要注意的地方。
为什么这样设计见[多账户](../design/accounts.md#95-落地顺序)和[拉取的一轮](../design/drivers.md#1013-imap-拉取的一轮提交顺序与不丢信)。

前提：已经挂好 KEK（[第三方机密](secrets.md)），`serve` 在跑。

下面用主机上的命令演示。同样的事也能在 WebUI 里做：设置 →「外部邮箱」可以接入、编辑、测试、立即同步、暂停、恢复、移除邮箱，
看每个文件夹追新和回填到了哪里、哪几封没取到，以及设置每个账户（catch-all 也在这一页）谁能读。
那里的改动和命令记同样的审计、推同样的告警；接入邮箱、给新密码或新服务器、让它多拉信、恢复、让某个主体能读，
都要先用 passkey 再验一次，只收窄的改动不用。密码只能写不能读，页面上只显示它封存的时间和能不能打开。

## Gmail

1. **打开两步验证**：Google 账户 → 安全性 → 两步验证。没有两步验证就没有 app password。
   Google Workspace 的账户还要管理员允许：管理员可以关掉 app password，也可以要求只用安全密钥登录，这两种情况下这一步做不了，
   要等 OAuth
2. **生成 app password**：Google 账户 → 安全性 → 两步验证 → 应用专用密码，起个名字（比如 `yuuka`），记下那 16 个字母。
   它能读写整个邮箱，当成主密码一样保管；不用了就在同一个页面撤销
3. **确认 IMAP 开着**：Gmail 设置 → 转发和 POP/IMAP。现在的 Gmail 默认开着
4. **加账户**，密码从标准输入给，不进 shell 历史：

   ```bash
   yuuka account add gmail --imap --preset gmail --address me@example.com --password-file -
   ```

   用 Docker Compose 时：`docker compose run --rm -T yuuka account add gmail --imap --preset gmail --address me@example.com --password-file - < app-password`
5. **测一下登录**，不取信：

   ```bash
   yuuka account test gmail
   ```

   应该列出 `[Gmail]/All Mail`（界面语言不同名字也不同，SPECIAL-USE 是 `\All`）和它的信数
6. `serve` 会开始拉：先追新，再从新到旧回填旧信，每天最多下载 2GiB（Gmail 自己每天的 IMAP 下载量有上限，碰到了也会停到第二天）。
   `yuuka account ls` 看进度。大邮箱的回填要好几天，这期间新信照常进来

几处 Gmail 特有的：

- **只拉「所有邮件」**：Gmail 的标签在 IMAP 里是文件夹，同一封信出现在好几个文件夹里。只拉 `\All` 每封信只拉一次，标签另外记下来。
  不要用 `--mailboxes` 再加 `INBOX` 之类，那只会让同一封信多拉一遍
- **发出去的信也在里面**：「所有邮件」包含已发送，它们照样入库，但不推送通知。草稿不入库
- **用 Gmail 发信**要另外建一个出口（`smtp.gmail.com`，465），同一个 app password 再给一次：
  密码只会交给为它加密的那台服务器，所以 IMAP 和 SMTP 各存一份（[为什么](../design/secrets.md#178-m35-的形状)）

## 可信接收方

拉来的信头里有好几条 `Authentication-Results`，只有邮箱服务商自己加的那一条可信，其余的谁都能写。
`--trusted-authserv-id` 告诉 yuuka 只信哪一条（[为什么](../design/drivers.md#1011-认证事实只信可信接收方)）。

**缺省为空**：这个账户的信一律算「未验证」，前缀的泄漏、仿冒判定不报，发件方 `dmarc=fail` 的信也不降级。
这是安全的缺省，代价是这些检查对这个账户不起作用。

Gmail 的值预计是 `mx.google.com`，但还没在真实的信上核实过，所以预设不填它。要设的话先打开一封拉来的信看原文，
确认最上面一条 `Authentication-Results:` 后面紧跟的那个名字，再：

```bash
yuuka account set gmail --trusted-authserv-id mx.google.com
```

**只对之后拉来的信生效**。已经入库的信保留原来的判定，要重算就跑一次 `yuuka reparse`（`--since` 限定范围，[维护](cli.md#维护)）。
改它、清空它都会给管理员推一条安全告警：它决定哪一条头被当成事实。

## 谁能读

拉取账户默认谁都不能读，除了加它时 `--read` 列出的主体（不写时是库里唯一的那个人）。给 agent 读：

```bash
yuuka account access gmail --principal agent:arisu --read
```

`yuuka account access gmail` 列出现在谁能读。agent 能读一个账户，就能在列表、搜索和线程里看到它的全部信，
`list_accounts` 里有这个账户的名字和地址，搜索时可以用 `account:gmail` 只查它；
它永远看不到这个账户的服务器、登录名、同步进度和密码。

只取了头部的信（比来源的大小上限大，`--max-message-size`）在读信界面标「只有头部」，正文不在 yuuka 里。

## 登录被拒

服务器拒绝了密码时，yuuka **不重试**，直接暂停这个账户：用错误的密码反复登录 Gmail 会让账户被临时锁住，
重试只会更糟。`account ls` 的 STATE 显示 `paused: login refused`，`serve` 的日志每天报一次 ERROR，直到你处理。
怎么处理见[排障](troubleshooting.md#外部邮箱停了paused-login-refused)。
