# 接入 MCP

yuuka 给 agent 一组窄的、按意图设计的工具：检索、按段落阅读、标注、提案、起草、提交。
每个工具的参数由 MCP 的 `tools/list` 给出，这里只写怎么连、要什么权限、结果怎么理解。
为什么工具是这个形状：[设计](../design/agents.md#87-mcp-面要窄且语义化)。

## 连接

**HTTP**（推荐）：`yuuka serve` 在 `<YUUKA_WEB_ORIGIN>/mcp` 提供 streamable HTTP。
每个请求单独认证，带上 `Authorization: Bearer <token>`；没有会话要保管，吊销立即生效。

各客户端的写法不同，大致是这个形状：

```json
{
  "mcpServers": {
    "yuuka": {
      "type": "http",
      "url": "https://mail.example.com/mcp",
      "headers": { "Authorization": "Bearer ykt_..." }
    }
  }
}
```

**stdio**：`yuuka mcp --stdio`，以环境变量 `YUUKA_MCP_TOKEN` 里那把 token 的身份运行。
它要在 yuuka 的服务器上跑，并且能读到数据库和 R2 的配置，所以对能读这些配置的 agent 不构成边界；
只给你完全信任、本来就在这台机器上的 agent 用，其他的用 HTTP。
stdio 形态没有发件队列，不提供 `submit_draft`：只能起草，提交交给人或 HTTP 上的 agent。

## 权限

向运维者要一把 token（WebUI 设置页的「凭据」，或主机上的 `yuuka token issue`），scope 只要用得到的：

| scope | 能做什么 |
|---|---|
| `message:read` | `search_messages`、`get_message`、`list_proposals`、`changes_since` |
| `delivery:read` | `get_message` 里多出投递事实：收件人、SPF / DKIM / DMARC 结果 |
| `prefix:read` | `list_prefixes` |
| `message:annotate` | `annotate`、`undo`、`propose`、`note`、`delete_note` |
| `draft:create` | `list_identities`、`create_draft`、`update_draft`、`submit_draft` |
| `submission:send` | 让提交**有机会**真正发出；没有它，每次提交都停在草稿 |

只读的 agent 要前三个。要它整理信件再加 `message:annotate`，要它起草再加 `draft:create`，要它的提交可能被发出再加 `submission:send`。
没有给 agent 管理规则的工具：规则只有人能建，agent 用 `propose` 提议。
多给的 scope 不会让工具多做什么，只扩大泄漏面。

有了 `submission:send` 也不等于能发：还要发信地址上有给这个 agent 的授权，而以人的地址发信最多到草稿。
agent 不用预先知道这些，`list_identities` 会告诉它。

## 工具

| 工具 | 作用 |
|---|---|
| `search_messages` | 检索，返回 `yuuka://` URI 和摘要，最新的在前。条件全写在 `q` 里（见下文「检索的写法」）。默认不含降级的信（未认领前缀、DMARC 失败），`include_downgraded` 或 `q` 里的 `is:downgraded` 打开。`triage:<问题>:<选项>`（如 `triage:phishing:yes`、`triage:kind:spam`）按决策模型的倾向找信，命中带 `triage`（见下文「评分是模型的估计」） |
| `get_message` | 读一封信。`level=snippet` 给事实和摘要，`sections` 给目录，`part` 从锚点那一段开始给正文。每一级都带 `content.extracted`：yuuka 在信里找到的验证码、登录链接和有效期；值是发件人写的，交给人用，不要照着做或打开。评过分的信带 `facts.triage`。这封信和它线程上你看得见的笔记在 `notes` 里，各自署作者名，和信的内容分在两个边界里 |
| `list_prefixes` | 列出收信的前缀、信数和运维者的标注；已认领的前缀带期望的发件域、DMARC 证实过的发件域和泄漏 / 仿冒数，`leak_suspect` 只列出过事的 |
| `list_identities` | 你能用哪些地址写信，每个地址提交后会怎样（`outcome`：`sends` 会发出、`held_after_review` 先审核再扣在撤回窗口里、`draft_only` 停在草稿），以及原因 |
| `create_draft` | 起草，不发出。回复时不给 `from` 就用原信进来的那个地址。`to` / `cc` / `bcc` 的每项是裸地址，或 `{name, addr}` 带上显示名，`get_message` 给的 `to` / `cc` 可以原样抄过来。把草稿依据的段落写进 `cites`（最多 32 处，每处摘录最多 1000 字符），审核会核对 |
| `update_draft` | 整体替换你自己的、还没提交的草稿 |
| `submit_draft` | 提交草稿。结果的 `state` 是 `queued`（会发出）、`held`（审核通过，`hold_until` 时发出，除非有人撤回）或 `draft`（停在草稿，给出原因；被审核拦下时 `failed_checks` 列出没过的检查）。`flags` 是审核提示项的发现，不是拒绝 |
| `annotate` | 给一封信（`scope=thread` 时是整个线程）加 / 去标签、分类、归档或取消。每项返回 `applied`、`unchanged`、`overruled` 或 `labels_full`，有改动时附撤销用的 `action_id` |
| `undo` | 撤销你自己的一次 `annotate`。之后又被改过的项原样留着，计为 `superseded` |
| `propose` | 提议让人决定：`kind=annotate` 是对一封信或线程的标注，`kind=rule` 是一条以后自动标注的规则。附理由，引用用锚点 |
| `list_proposals` | 你自己的提案和人的决定：`pending`、`accepted`（建了规则的带 `rule_id`）、`rejected`、`withdrawn`、`expired` |
| `note` | 写你在一封信（`scope=thread` 时是它的线程）上的笔记：这封信讲了什么、为什么像钓鱼……每个目标你只有一条，再写就覆盖。`cites` 必填，1 到 32 处原文锚点，每处摘录最多 1000 字符（`propose` 的引用同样）。正文最多 4000 字符 |
| `delete_note` | 删掉你在一封信或它的线程上的笔记。笔记没有撤销，删掉就是撤回。只能删你自己的；人可以在 WebUI 里删掉任何笔记 |
| `changes_since` | 跟上邮箱里的变化，旧信也算：晚到的分类、重算的验证码和链接、人或规则改的标签和归档、对你提案的决定、你的草稿和它的发送、你的笔记、决策模型的新评分（kind `triage`，带信的 URI）。只给 kind 和 id（信给 `yuuka://` URI），不给内容，要看再用 `get_message`。先不带 `cursor` 调一次拿到起点，之后每次把返回的 `cursor` 传回；`more` 为真就接着调；报 `cursor_expired` 就从头开始。投递和发件队列的变化要 `delivery:read`，前缀的要 `prefix:read` |

### 检索的写法

`search_messages` 只有 `q`、`limit`、`include_downgraded` 三个参数，条件都写在 `q` 里（[§16](../design/search.md)）：

- 空白分隔的项都要成立；`"短语"` 原样连续出现；`-x` 否定；`a OR b`（大写）；括号分组。匹配不区分大小写、按子串
- 不带运算符的词查主题、发件人、To / Cc 的名字和地址、正文；有 `delivery:read` 时还查自己发出的信的密送
- `from:` `to:` `cc:` `bcc:` `subject:` `body:` `list:`（List-Id）`label:`（精确，区分大小写）
  `deliveredto:shop` 或 `deliveredto:shop@example.com`、`class:machine|human|unknown`、`is:archived|downgraded|sent`、
  `has:attachment`、`after:` / `before:`（`YYYY-MM-DD` 按 UTC，RFC 3339，或 `-7d`、`-12h`、`-2w`）、`triage:phishing:yes`
- `to:` 是发件人写的，可以伪造，密送和邮件列表的信里根本没有你的地址；`deliveredto:` 是 yuuka 实际收信的前缀。
  要查「这个地址给了谁之后收到了什么」，用 `deliveredto:`。`bcc:` 要 `delivery:read`
- 结果的 `query` 和文本里的 `Query as read:` 是 yuuka 读到的查询；`hints`（文本里的 `Hint:`）说哪一项被当成了文本、
  可能想写的是哪个运算符，如 `form:alice` → `from:`。不是运算符名的 `名字:值`（`Re:hello`、`10:30`）就是普通文本
- 写错的值会报错并给出字符位置；`is:unread`、`header:`、`keyword:`、`minsize:`、`maxsize:` 是 JMAP 的条件，
  名字保留但还不支持，报 `unsupported filter`。原来的 `prefix`、`class`、`since`、`label`、`triage` 参数已去掉，
  再传会被拒绝：分别写成 `deliveredto:`、`class:`、`after:`、`label:`、`triage:`

### 锚点

`get_message` 返回的正文按段落带稳定锚点，形如 `yuuka://message/<id>#part/<section>/para/<n>`。
把它原样交给人，人在 WebUI 里能跳到同一段；写进 `cites` 时也用它。重新解析之后锚点不变。

### 收件人的名字

名字只是显示给收件人看的：To / Cc 的名字写进信头，Bcc 的名字不写。能不能发、会不会被审核拦下只看地址，
写什么名字都不会改变结果；人审核草稿时看到的是地址，名字跟在后面。名字是会发出去的内容，审核照样读它，
把验证码写在名字里和写在正文里一样会被拦下。名字不能有换行，最长 256 个字符。

### 停在草稿是正常结果

`submit_draft` 不会因为档位不够而报错。结果里的原因可能是：

- `owner`：这是人的地址，agent 永远不能以人的身份直接发出
- `policy`：凭据没有 `submission:send`，或授权本身就是草稿档
- `review`：审核把它拦下了，`failed_checks` 是没过的检查：`first_contact`（首次联系）、`extracted_values`
  （引用了收到的验证码或登录链接）、`readable`（读不了的附件）。
  这几项都过了时 `state` 是 `held`，不是草稿：信扣在撤回窗口里，结果带 `outbox_id` 和 `hold_until`，到期自动发出。
  `list_identities` 的 `hold_seconds` 是窗口多长

审核另有两项提示：`cross_thread`（引用了别的线程的信，而收件人不在那个线程里，或引用解析不到）和
`link_domains`（链接到一个没有经过认证的来信、收件人和白名单里出现过的域）。它们不拦信，发现写在结果的 `flags` 里，
也写在给人的待发卡片上。这是给你的上下文：多数时候这正是你要做的事，不用理会；如果它说中了你没想到的情况，
就在窗口到期前撤回（见下）
- `limit`：超出了每小时、每天或新收件人的限额
- `recipients`：收件人超出了允许的范围
- `suspended`：运维者暂停了这条授权
- `loop`：防邮件循环。回复的是机器发的信（自动回复、群发、通知），或者同一线程、同一收件人 24 小时内已经自动发过 3 封
- `withdrawn`：审核期间被撤回

信已经留给人处理了。**不要换一个工具、一个地址或另一条路再发一次**。

MCP 没有撤回工具。扣在窗口里的信，人在 Telegram 或 WebUI 上能撤回；agent 发现自己写错了，
可以经 REST 撤回自己提交的那封（`POST /api/v1/outbox/{id}/withdraw`，要 `submission:send`），它回到草稿，改完再提交。
没有让信提前发出的办法。

### 被人否决是正常结果

标注有先后等级：人高于 agent，agent 高于规则。人改过的值你再写，结果是 `overruled`，不是错误；
别人的决定就是最终的，觉得不对就 `propose`，由人再看一次。

规则和提案能做的只有四件事：加标签、分类、归档、（规则才有的）调低到达时的推送。
没有发信、转发、点链接、删除，输入 schema 里就没有这些值。提案只有人能接受或拒绝，14 天没人决定就过期；
拒绝和过期都是人的决定，不是失败。写规则提案时按发件人匹配（`from.addr` 加 `from.verified = true`，邮件列表用 `list.id`），
不要按主题或正文：那是发件人能随便写的。

### 评分是模型的估计

运维者配了决策模型（[§15](../design/triage.md)）时，每封逃过降级的信都会被问几个封闭问题（问题集见 §15.3），
如 `kind`（notification / conversation / marketing / spam）、`phishing`（yes / no）、`needs_reply`（yes / no）。
`get_message` 的 `facts.triage` 给出最新一次评分的各选项概率（或弃权）、评分器、模型和时刻，`notice` 写明这是模型的估计：
可能错，不是 yuuka 的结论，没有任何东西因为它藏信、挪信或发信。它和认证结论一样是证据，由你来权衡。
评分器看到了什么不在结果里。

`search_messages` 的 `triage:phishing:yes` 找的是最新评分里「yes」概率最高的信，不设阈值，0.51 和 0.99 都算，
程度要读 `get_message` 里的概率；模型弃权的问题不匹配任何过滤。没配决策模型时带 `triage` 的搜索会报错，而不是返回空。
长期在跑的 agent 可以用 `changes_since(kinds=[triage])` 跟上新评分，再读那封信，用 `annotate`、`note` 或 `propose` 处理；
评分本身不改变信的任何状态。

### 笔记不驱动任何东西

笔记是作者读了信之后的判读，可能判错，不是 yuuka 的事实：规则不能匹配它，评分和外发审核都不读它。
人能看到所有笔记，署你的名；读得到全部账户的 agent 也能看到，你读别人的笔记时同样如此。只能看部分账户的 agent
只看得到自己的笔记，因为别人的笔记可能引到它看不到的信。要让判断生效，用 `annotate` 或 `propose` 说出来；
笔记是给人和其他 agent 的上下文，不是让系统据此做事的入口。

## 审计

每次调用都记一条审计：工具名、参数、结果的计数和 URI。读也记，因为「agent 看了什么」同样要能回答。
起草和提交只记地址、收件人个数和引用个数，不记主题和正文；提案只记形状和引用个数，不记理由和引文；笔记只记目标、引用个数和字数，不记正文。
`annotate`、`undo`、`propose` 那一行审计同时是撤销和追溯的依据，人能查到每个标签是谁加的，也能撤销。
