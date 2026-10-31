# 管理命令

`yuuka` 的子命令按要做的事分组列在下面。这些命令在服务器上直接跑，不登录、不冒充任何人：
能在这台机器上跑它们，就是最高权限。每次改动都记进审计（`actions`），带上执行者在主机上的用户名。
[`cmd/yuuka`](../../cmd/yuuka/) 里的测试会核对每个子命令和参数都在这一页出现；
不带参数运行某个命令会打印它的用法。

用 Docker Compose 时在 `deploy/` 目录里加上前缀，复用 `yuuka` 服务的环境：

```bash
docker compose run --rm yuuka prefix ls
```

下文省略这个前缀。各命令要哪些环境变量见[配置参考](config.md#各命令要什么)。

## 服务本身

```bash
yuuka serve
```

常驻进程：webhook、定期补收、WebUI、REST、MCP，配了出口就发信，配了提交口就监听 SMTP。
收到停止信号后不再领新的活，等进行中的发送拿到最终回复再退出，所以给它留一分钟左右的停止宽限期。

## 第一次登录

```bash
yuuka admin enroll [--name human:<operator>] [--ttl 15m]
```

打印一条一次性的 passkey 注册链接和一组（8 个）离线恢复码。在浏览器里打开链接、注册 passkey，之后在 WebUI 里添加更多设备。
`--name` 缺省时用库里唯一的那个人，没有就建 `human:operator`；`--ttl` 是链接的有效期。

**恢复码离线保存**，库里只有哈希。passkey 全丢时，在 `<YUUKA_WEB_ORIGIN>/recover` 用一个恢复码换一次新的注册；
恢复码不能直接登录。还能登上服务器的话，再跑一次 `admin enroll` 更简单，它同时换一组新的恢复码，旧的作废。

## 前缀

catch-all 域上的任何地址都收，第一次见到的前缀自动登记为 `discovered`。发往 `discovered` 前缀的信
多半是字典攻击，只静默入库：不推送通知、不进聚合视图。把你给出去过的地址标成 `claimed`，它的信才正常显示和推送。
认领立刻对已有的信生效，但不会补推旧信；地址给出去时就认领也行，不必等第一封信
（[为什么分两态](../design/data-model.md#43-垃圾判定要有来源)）。

```bash
yuuka prefix ls [--state claimed|discovered] [--zone example.com]
yuuka prefix claim shop@example.com
yuuka prefix unclaim shop@example.com
yuuka prefix expect shop@example.com --domains example.net[,example.org]
```

`ls` 列出每个前缀的首次、最近一次出现和投递数；已认领的前缀还有期望的发件域（EXPECTED）、
DMARC 证实过的发件域和各自的信数（OBSERVED，不在期望里的前面带 `!`）、泄漏数（LEAKS，括号里是发件方没有证实的）和仿冒数（SPOOFS）。

`expect` 写下这个地址给了谁，替换原来的列表，`--domains ''` 清空。只对已认领的前缀有效。
写子域也行，存下来的是组织域（`mail.example.net` 记作 `example.net`），命令会说一声。
之后发件方 DMARC 通过但不在列表里的信算泄漏，自称是列表里的域却没过 DMARC 的算仿冒；
这对已有的信立刻生效，命令会顺带报出现在有多少
（[判定规则](../design/architecture.md#3-八条设计原则)）。没写期望的前缀不判定，`ls` 的 OBSERVED 列可以拿来定期望。

## 出口与第三方机密

出口（transport）是 yuuka 把信交出去的 SMTP 服务器：主机、端口、用户名、密码和 caps 都存在库里，
密码用 KEK 加密（[配置](config.md#第三方机密的加密钥匙)、[为什么](../design/secrets.md#17-第三方机密与渠道进库加密信任根留在主机)）。
写密码的命令要读到 `YUUKA_KEK_FILE`；`ls` 和 `rm` 不需要。从环境变量里的出口升级上来、换 KEK，见[第三方机密](secrets.md)。

```bash
yuuka transport add cloudflare --host smtp.mx.cloudflare.net [--port 465] --username <用户名> \
  --password-file /path/to/password --caps cloudflare
yuuka transport add relay --host relay.example.com --auth none --caps standard
yuuka transport set cloudflare [--host H] [--port P] [--auth plain|none] [--username U] [--password-file PATH|-] [--caps C]
yuuka transport ls [--all]
yuuka transport test cloudflare
yuuka transport rm relay
```

- **密码只从文件或标准输入读**：`--password-file PATH`，或 `--password-file -` 从标准输入读（末尾的一个换行会去掉）。
  命令拒绝 `--password` 这类把机密写在参数里的写法：参数会留在 shell 历史和 `ps` 里。
  用 Docker Compose 时用标准输入最省事：`docker compose run --rm -T yuuka transport set cloudflare --password-file - < password`
- `--caps` 必填，没有默认值：`cloudflare`、`standard`（= `message-id,return-path`）、`none`，
  或 `message-id`、`return-path`、`dkim`、`status-query` 的组合。写错的后果是信被出口拒收，或者 yuuka 以为能对账而其实不能。
  不确定就从 `none` 开始；Cloudflare Email Sending 用 `cloudflare`。各个出口能做什么：[能力矩阵](../design/drivers.md#103-能力矩阵)
- `--auth` 缺省时有 `--username` 就是 `plain`（AUTH PLAIN），没有就是 `none`。只做隐式 TLS
- `add` 用于新名字、库里只有名字的占位出口，以及 `rm` 过的出口（重新启用，要给出全部参数）；已配置的出口报错，改它用 `set`
- `set` 只改给出的参数。**换了去向（主机、端口、认证方式、用户名任何一样）就要重新给密码**：
  密码只会交给当初为它加密的那台服务器，命令会提示。只改 `--caps` 不用给密码；给了 `--password-file` 就换成新密码
- `add` 和 `set` 先读出口、再按读到的版本写入。两次之间别人改过它时，命令报错、什么都不写，看一眼 `ls` 再跑一次
- `ls` 的 STATE 是 `configured`、`placeholder`（只有名字，不能发信）或 `disabled`（`ls --all` 才列）；
  SECRET、SEALED、KEY 是有没有密码、什么时候加密的、用哪把钥匙（`local:<kek_id>`）。从不显示密码本身，也不显示它的哈希
- `test` 用库里的配置连一次：TLS、`EHLO`、`AUTH`，然后断开，**不发 `MAIL`**，不会投递任何东西。换了密码之后用它确认，
  失败时报出停在哪一步（`connect`、`greeting`、`EHLO`、`AUTH`）。它要能解开密码，所以也要 `YUUKA_KEK_FILE`
- `rm` 是停用并抹掉密码。出口这一行留着，发送历史里的名字照样能对上；名字永远不能改，也不能给别的出口用。
  还把它写在出口链里的发信地址会列出来，用 `identity set` 换掉

每次 `add`、`set` 记一条审计（`transport.add`、`transport.set`），参数是名字、主机、端口、认证方式、用户名、caps，
以及新密码的 id、audience、包装器和钥匙引用；`rm` 记 `transport.rm`。审计里从不出现密码或它的哈希。

```bash
yuuka secret ls
yuuka secret check
yuuka secret rewrap
```

- `ls` 列出库里每一条第三方机密：id、类型、属于谁（OWNER，如 `transport cloudflare`、`channel telegram`）、audience（它只能交给哪个目的地）、
  包装器、钥匙引用（KEY）、加密时间。不需要 KEK
- `check` 逐条按「属于谁」重算目的地再解密，只报结果，不打印任何机密。RESULT 是 `ok`、
  `locked`（解不开它的钥匙没配：没设 `YUUKA_KEK_FILE`，或者旧 KEK 文件拿掉得太早）、
  `audience mismatch`（出口的去向在加密之后被改过，没经过 `transport set`：重新 `set` 一次密码）、
  `tampered`（密文对不上：被改过或损坏，当作安全事件查），以及少见的 `wrong kind`、`no owner`（库被绕过 yuuka 改过）。有任何一条不是 `ok` 时命令以非零退出
- `rewrap` 把每条机密的数据钥匙改用当前的 KEK 重新包装，密文本身不动；已经在当前 KEK 下的跳过。
  每改一条记一条审计（`secret.rewrap`，带新旧钥匙引用）。换 KEK 的步骤见[第三方机密](secrets.md#换-kek)

## 通知渠道

通知渠道现在只有 Telegram：一个渠道就是一个 bot。渠道上的**绑定**是某个人的一个 chat（私聊或群），
加上「谁按的按钮算数」；**订阅**是一个绑定收哪些事件。bot token 和出口密码一样用 KEK 加密存在库里
（[为什么](../design/secrets.md#174-表的形状为什么是这样)）。从 `YUUKA_TELEGRAM_*` 升级上来见[第三方机密](secrets.md#telegram-从环境变量升级)。

```bash
yuuka channel add tg --token-file /path/to/bot-token     # 或 --token-file - 从标准输入读
yuuka channel ls [--all]
yuuka channel bind tg [--principal human:<operator>] [--events mail.new,send.held] [--label 手机]
yuuka channel test tg [--binding <绑定 id>]
yuuka channel subscribe <绑定 id> grant.suspended
yuuka channel unsubscribe <绑定 id> mail.new
yuuka channel unbind <绑定 id>
yuuka channel rm tg
```

- `add`：token 从 [@BotFather](https://t.me/BotFather) 拿，**只从文件或标准输入读**，命令不接受写在参数里的 token。
  命令先用 `getMe` 问 Telegram 这是哪个 bot，记下它的数字 id 和用户名，再加密写入。要 `YUUKA_KEK_FILE`。
  名字已有的渠道再 `add` 一次是换 token（同一个 bot），或者重新启用 `rm` 过的渠道；一个启用中的渠道不能换成另一个 bot，
  先 `rm` 再 `add`。一个 bot 同时只能属于一个启用中的渠道（Telegram 只允许一个轮询者）。`serve` 15 秒内开始轮询新渠道
- `bind`：打印一个链接 `https://t.me/<bot>?start=<码>`（群用 `startgroup=`），在要收通知的 Telegram 账号里打开它、按 Start。
  码 10 分钟有效、只能用一次，库里只存哈希。`/start` 由正在运行的 `serve` 收（它要有 `YUUKA_KEK_FILE`），
  命令在终端上等着，收到后显示 **Telegram 报来的是谁**：名字、用户 id、chat id、私聊还是群，问你 `[y/N]`。
  不是你本人（链接被别人先点了）就答 N，什么都不会绑，码作废。答 y 才建绑定：
  那个用户在这个 chat 里按的撤回按钮算作 `--principal` 那个人的，bot 在 chat 里回一句已绑定，
  同时向这个人的其他绑定和管理员推一条安全告警。`--principal` 缺省时用库里唯一的那个人，只能是人（`human:`）；
  `--events` 缺省是全部四类；`--label` 缺省是 Telegram 报来的名字
- 事件：`mail.new`（新信摘要，只推这个人看得到的信）、`send.held`（扣留中的信，带撤回按钮）、`send.escalated`（被审核升级为草稿）、
  `grant.suspended`（授权被熔断）。`security`（新 passkey、恢复码、放宽、机密被篡改……）不能订阅也不能退订，
  总是发给每个管理员（现在是每个启用中的人）和事情有关的那个人的全部绑定
- `test` 往渠道的每个绑定（或 `--binding` 指定的一个）发一条测试消息，要 `YUUKA_KEK_FILE`
- `unbind` 立即生效：下一条通知就不再发过去，那个 chat 里已有卡片上的按钮也不再算数
- `rm` 停用渠道、抹掉 token、撤销它上面所有绑定。渠道名不能改

`ls` 列出渠道（bot、状态、版本）和绑定（id、渠道、人、chat、按钮用户、订阅、最后一次按按钮的时间）。
每次 `add` 记 `channel.add`（bot id、用户名、audience、包装器、钥匙引用，从不含 token），
`bind` 记 `channel.bind.begin` 和 `channel.bind.confirm`（chat、按钮用户），其余记 `channel.unbind`、`channel.subscribe`、
`channel.unsubscribe`、`channel.rm`。没有 `YUUKA_KEK_FILE` 时 `add`、`bind` 的安全告警发不出去，命令会说

## 发信地址与授权

发一封信要三样东西都齐：一个**出口**（[上一节](#出口与第三方机密)），一个绑在出口上的**发信地址**（identity），
以及一条**授权**说明谁能用这个地址发、能发到哪一步。

地址和授权也可以在 WebUI 的设置页里添加和修改，效果和下面的命令一样。签发、添加、调高这类放宽权限的操作，
提交前会再验一次 passkey，验过之后 5 分钟内不再询问，每次都推送告警
（[为什么](../design/principals.md#1310-管理面收紧一步完成放宽先再验-passkey)）。命令留给脚本和没有浏览器的时候。

```bash
yuuka identity add alerts@example.com --owner service:monitoring --transport cloudflare \
  [--display-name "Monitoring"] [--signature "..."] [--plain-strip-markup] [--account A]
yuuka identity add '*@example.com' --owner human:operator --transport cloudflare
yuuka identity set alerts@example.com --transports cloudflare,relay
yuuka identity ls [--all]
yuuka identity rm alerts@example.com
```

- `--transport` 是发信的出口，只接受库里已有的出口名（`transport ls` 里的，占位的也算；
  还在用环境变量时，`YUUKA_TRANSPORTS` 里的名字会自动建成占位），打错的名字报错而不是凭空建一个出口。
  `identity ls` 的 TRANSPORT 列是出口链，主出口在前
- `set --transports 主,备…` 整个替换出口链，最多 8 个，主出口在前；`add --transport NAME` 等于只有一个成员的链。
  换出口链改变了信的去向，记一条审计（`identity.route`，带新旧两条链）
- 发信时按链的顺序用第一个能用的出口：主出口连不上或认证被拒而暂停时、被 `transport rm` 时、密码上锁时、还只是占位时，走下一个。
  信已经交给出口、只是没拿到结论（`unknown`）时不换出口重发。入队时按链上所有已配置出口里最严的 caps 去掉信头，
  所以链上有 `cloudflare` 这类出口时，经主出口发的信也不带自己的 `Message-ID`
  （[为什么](../design/drivers.md#1022-出口链与执行器)）
- `--owner` 是这个地址属于谁：`human:<name>`（必须已存在）、`agent:<name>` 或 `service:<name>`
- `*@example.com` 让这个人能以该域的任意前缀发信
- `--display-name` 是信头 `From` 里的显示名，会覆盖 SMTP 客户端自己写的
- `--signature` 是附在撰写的信后面的 Markdown；`--plain-strip-markup` 让纯文本部分去掉 Markdown 标记
- `--account` 只在库里有多个账户时需要
- `rm` 是停用，不删除；`ls --all` 连停用的也列出

```bash
yuuka grant set --identity alerts@example.com --principal service:monitoring \
  --policy auto --recipients allowlist --allow ops@example.com,example.org
yuuka grant ls [--identity A] [--principal service:monitoring]
yuuka grant rm --identity alerts@example.com --principal service:monitoring
```

`--policy` 是发到哪一步：

| 档位 | 提交之后 |
|---|---|
| `none` | 拒绝 |
| `draft` | 停在草稿，等人在 WebUI 里确认发送 |
| `review` | 先过审核：首次联系、验证码和登录链接外泄、读不了的附件会拦下，停在草稿；其余扣留一个撤回窗口，到期发出。跨线程引用、陌生链接域不拦，写在待发卡片和结论里，供撤回时参考 |
| `auto` | 直接发出，受收件人范围和限额约束 |

`--recipients` 是能发给谁：`thread`（只能回复线程里已有的人）、`allowlist`（`--allow` 里列的地址和域，整体替换，给空值清空）、`any`。

限额：`--max-per-hour`、`--max-per-day`、`--max-new-recipients-per-day`，给数字或 `default`。
默认值按主体类型：人不限；service 每小时 30、每天 200、每天 5 个新收件人；agent 每小时 10、每天 50、每天 5 个新收件人。
超出档位或限额的信不会被拒绝，而是停在草稿等人看。`--hold-seconds` 是 `review` 档的撤回窗口，给秒数、`0`（不设窗口）或 `default`（600 秒）。
窗口里的信在 WebUI 或 Telegram 卡片上可以撤回（[Telegram 怎么配](config.md#通知)），撤回后回到草稿。

`--suspend` 立即停用一条授权，之后每次提交都停在草稿；`--resume` 解除。它也是紧急开关：
已经扣留在撤回窗口里的信到期时不会发出，而是退回草稿。一条授权的信一天内被人撤回两次，yuuka 会自动暂停它，
同样用 `--resume` 恢复。

几条要知道的：

- `set` 只改你给出的参数，其余保持原值；新授权从 `none` + `thread` 起步
- `rm` 之后适用的是默认规则，不一定是「什么都不能做」，命令会把结果打印出来。
  默认规则：人对自己的地址是 `auto`；agent 或 service 对自己拥有的地址是 `review`；agent 对人的地址是 `draft`；其余 `none`
- **非本人以人的地址发信，上限是草稿**，写不进更高的档位。给服务用的地址，`--owner` 就写这个服务
  （[为什么](../design/principals.md#135-identity--principal-授权)）

## 规则

规则在信到达时给它打标签、改判、归档、定推送级别；它不能发信、转发、点链接或删除，格式里根本没有这些动作。
匹配条件、字段和动作的完整说明见 [`internal/rules`](../../internal/rules/doc.go)，
为什么这样设计见[设计文档](../design/agents.md#85-协作的单位是任务不是邮件)。

```bash
yuuka rule ls [--all]
yuuka rule show <id>
yuuka rule add --spec rule.json [--name "银行"] [--priority 0] [--disabled]
yuuka rule set <id> [--spec rule.json] [--name N] [--priority P] [--enable | --disable]
yuuka rule rm <id>
yuuka rule apply <id> [--since 2026-01-01] [--dry-run]
yuuka rule revert <id> [--since 2026-01-01]
```

`--spec` 是规则本身：一个文件、`-`（从标准输入读），或者直接写 JSON：

```json
{"v": 1,
 "match": {"all": [{"field": "from.domain",   "op": "eq", "value": "example.net"},
                   {"field": "from.verified", "op": "eq", "value": true}]},
 "actions": [{"kind": "label", "label": "bank"}, {"kind": "archive"}]}
```

- 保存时校验，存下的是规范化后的形式（地址值折成小写）。按 `From` 匹配却不要求 `from.verified`、
  动作里又有标签或推送时会给警告：`From` 谁都能伪造
- 多条规则按 `--priority` 从小到大、同级按 id 对同一封信求值，互相看不到对方的效果。
  分类、归档、推送级别取第一条命中的，标签取并集
- 人和 agent 的决定优先于规则：你删掉规则加的标签，之后再跑规则也不会加回来
- 规则只看收到的信，不看自己发出的。推送级别只能把通知调低或关掉，被降级的信无论规则怎么写都不推送
- `ls` 的 STATE 有 `enabled` `disabled` `deleted`，以及 `unsupported`（这个版本看不懂的规则，永远不匹配）
- `set` 只改给出的参数，每次改动都让版本号（REVISION）加一；`--disable` 停用，已经做过的标记保留
- `rm` 是软删除：规则停止运行，它做过的事留着、历史可查

规则只对之后到达的信生效。要用在已有的信上，显式地跑：

- `apply` 让一条规则过一遍 `--since` 之后收到的信（缺省为全部），尊重人和 agent 的决定，不推送。
  `--dry-run` 只数会改多少封，什么都不写。中途中断的话再跑一次即可，已经做过的不会重复
- `revert` 撤回这条规则（含已删除的）对 `--since` 之后的信做过的全部改动；之后又被别人改过的保持现状，命令会报出数量

## 凭据

agent 和其他服务用凭据访问 yuuka。人没有长期凭据，只用 passkey 登录。
WebUI 设置页的「凭据」一节也能签发（先再验一次 passkey），规则和下面相同，秘密同样只显示一次。

```bash
yuuka token issue --principal agent:arisu --scope message:read,delivery:read,prefix:read [--label laptop] [--ttl 720h]
yuuka token issue --kind smtp --principal service:monitoring [--label alertmanager] [--ttl 720h]
yuuka token ls [--principal agent:arisu]
yuuka token revoke <凭据 id>
```

- 默认的 `--kind token` 是 REST 和 MCP 用的 bearer token；`--kind smtp` 是 SMTP 提交口的密码，
  用户名是命令单独打印出来的凭据 id
- `--scope` 必须逐个写明，没有默认值，也没有「全部」。每个 scope 的含义和 agent 通常要哪些，见[接入 MCP](../integrate/mcp.md#权限)。
  SMTP 密码的 scope 固定是 `submission:send`，不用写
- `--principal` 只能是 `agent:` 或 `service:`，不存在就建
- 秘密**只打印一次**，单独一行写到标准输出，可以直接重定向进文件。丢了只能吊销重发
- `--ttl` 缺省为不过期；`ls` 里的最后使用时间让闲置的那把看得见
- `revoke` 立即生效，对已经开着的 SMTP 会话也是

## 发件队列

```bash
yuuka outbox ls [--state unknown] [--limit 50]
yuuka outbox resolve <id> --sent | --resend | --abandon
```

`ls` 列出状态、收件人、尝试次数和最后的错误，最新的在前。状态有 `held` `queued` `sending` `sent` `partial` `unknown` `failed` `dead` `withdrawn`。

`unknown` 是「可能已经发出」：信已经交给出口，但没拿到结论（比如进程在等最终回复时崩了）。
yuuka 不会自己重发，因为那可能让对方收到两封。去出口的日志或收件人那边确认之后：

- `--sent`：确实发出了
- `--resend`：没发出，或者接受可能重复，再发一次
- `--abandon`：放弃，记为 `dead`

## 维护

```bash
yuuka migrate
yuuka reconcile [--full]
yuuka reparse [--since 2026-01-01]
yuuka version
```

- `migrate`：把表结构升到当前版本。可以重复跑，两个同时跑也安全。升级流程见 [deploy/README.md](../../deploy/README.md)
- `reconcile`：立即扫一遍投递标记，把 webhook 漏掉的信补进来（`serve` 本来就定期做）。
  `--full` 还把 R2 里的每个原件和库对一遍，用在从备份恢复之后
- `reparse`：从原件重新解析 `--since` 之后收到的信（RFC 3339 或 `YYYY-MM-DD`，缺省为全部），用在修了解析器之后。
  它只重建从内容派生的部分，不碰你做过的标记、改判和认领

## 分诊评分

```bash
yuuka triage status
yuuka triage rescore --since 2026-10-01
yuuka triage report [--since 2026-10-01]
```

要先配好评分器（[配置](config.md#分诊评分器)）。

- `status`：当前的评分器名字、问题集版本、地址和是否托管；回溯期内评了多少封、还有多少封等着评；
  再发一个不含任何信件的探测请求，看评分器答不答、回报的是哪个模型。探测失败时命令以非零退出
- `rescore`：把 `--since` 之后收到的信（RFC 3339 或 `YYYY-MM-DD`，必填）用当前评分器重新评一遍，
  写新的评分，旧的留着。换了模型、或升级后问题集换了版本，又想看回溯期以前的旧信的新分数时用（回溯期内的 `serve` 会自己补）。评分器连续失败时中途停下，已评的保留
- `report`：按问题统计当前评分器的最新评分：每个问题各选项各有多少封（取概率最高的那个）、多少封弃权。
  这不是阈值，也没有人工判断，只是分布；`--since` 缺省为全部。REST 的 `GET /api/v1/triage/report`（只给人）是同一份计数。
  要看具体是哪些信，用消息列表或搜索的 `triage=phishing:yes`、`triage=kind:spam` 过滤

## MCP（stdio）

```bash
YUUKA_MCP_TOKEN=... yuuka mcp --stdio
```

在标准输入输出上提供 MCP，给跑在同一台机器上的 agent 用。它要能读到数据库和 R2 的配置，
所以对能读这些配置的人不是安全边界；不可信的 agent 用 HTTP 形态。见[接入 MCP](../integrate/mcp.md)。
