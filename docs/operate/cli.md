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

## 发信地址与授权

发一封信要三样东西都齐：一个**出口**（[配置](config.md#发信出口)），一个绑在出口上的**发信地址**（identity），
以及一条**授权**说明谁能用这个地址发、能发到哪一步。

```bash
yuuka identity add alerts@example.com --owner service:monitoring --transport cloudflare \
  [--display-name "Monitoring"] [--signature "..."] [--plain-strip-markup] [--account A]
yuuka identity add '*@example.com' --owner human:operator --transport cloudflare
yuuka identity ls [--all]
yuuka identity rm alerts@example.com
```

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

## MCP（stdio）

```bash
YUUKA_MCP_TOKEN=... yuuka mcp --stdio
```

在标准输入输出上提供 MCP，给跑在同一台机器上的 agent 用。它要能读到数据库和 R2 的配置，
所以对能读这些配置的人不是安全边界；不可信的 agent 用 HTTP 形态。见[接入 MCP](../integrate/mcp.md)。
