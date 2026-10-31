# 检索：查询的表示与语义

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 16. 检索

> 状态：设计稿，未实现。现状是 [`store.SearchQuery`](../../internal/store/search.go)：
> 自由文本按空白切词，每个词在「主题 + 发件人 + 正文」的拼接上做子串匹配，其余条件是并列的结构化字段。
> 存储和索引的取舍（trigram、不用外置引擎、暂不做向量）在 §4 的「全文检索」，本节不重复。

现状有三个问题：

- **能找的东西说不出来**。store 已经能按前缀、分类、时间、标签、归档、评分倾向过滤，
  但 WebUI 的搜索框只传 `q`，REST 和 MCP 又各自暴露了不同的子集（REST 没有前缀和时间，MCP 没有归档和翻页）。
  同一个问题，agent 搜得到，人却搜不到
- **没有字段、没有否定和「或」**。`from:`、`-词`、`"短语"` 是任何邮件客户端的基本功。
  引号现在被当成普通字符，`"foo bar"` 会变成 `"foo` 和 `bar` 两个词
- **搜索会丢掉所在的视图**。在一个标签或前缀里输入关键词，结果来自全部邮件

### 16.1 以 JMAP 为准

查询的语义直接用 JMAP（RFC 8621 §4.4 的 `Email/query`），不另起一套：

- JMAP 定义了的能力，语义按 RFC 来：有哪些条件、字符串怎么匹配、`before` / `after` 的开闭、`AND` / `OR` / `NOT` 的含义
- JMAP 没有的是 yuuka 自己的事实：实际投递到的前缀、降级、分类、标签、归档、评分倾向。
  它们作为扩展条件加进同一棵过滤树
- M4 的 `Email/query`（§5）把 JMAP 的 `Filter` 直接解码成这棵树，复用同一个编译器，
  网关里不必再写一套语义。现在先把形状对齐，到时候就不会出现「WebUI 搜得到、JMAP 客户端搜不到」的分歧

不追求 Gmail 兼容。

#### 两种写法，各按读者的习惯

同一个条件有两个名字，在 16.3 的表里一一对应：

- **文本写法**给搜索框和 agent 用，按邮件客户端的习惯：小写、短，布尔条件写成 `is:archived`、`has:attachment`。
  人在框里打得出来；agent 对这类写法很熟（Gmail、Fastmail、notmuch 都长这样），不用先学
- **JSON 写法**就是 JMAP 的 `FilterCondition`，按 JMAP 的习惯：驼峰属性名，布尔值写 `true` / `false`。
  JMAP 定义了的条件用 RFC 的名字（`hasAttachment`），扩展条件按同样的风格起名（`deliveredTo`、`archived`），
  M4 时挂在 yuuka 自己的 capability 下面

没有让文本写法也用驼峰名（`isArchived:true`），因为人打不顺；也没有让 JSON 写法用 `is:` 一类前缀，
那样 JMAP 客户端读不懂。两种写法的名字不同，但底下是同一棵树、同一种含义。

### 16.2 一棵过滤树，一种文本写法

```
文本 q ──解析──▶ Filter（JMAP 形状的树）──编译（带 Viewer）──▶ SQL
JMAP Filter JSON（M4）──解码──┘
```

- **Filter 是唯一的内部表示**。节点是 `AND` / `OR` / `NOT`（JMAP 的 `FilterOperator`，`NOT` 的意思是「其中没有一个成立」），
  叶子是一个条件。它是不碰数据库的纯数据：解析和规范化输出放在一个独立的包里，编译成 SQL 在 store
  （和规则的分工一样，§8.5）
- **搜索只有 `q` 一个入口**。REST 的 `GET /api/v1/search` 只留 `q`、`downgraded`、`tz`、`cursor`、`limit`；
  MCP 的 `search_messages` 只留 `q`、`include_downgraded`、`limit`。现在 REST 上的 `label` / `archived` / `triage`、
  MCP 上的 `prefix` / `class` / `since` / `label` / `triage` 都去掉，改写进 `q`。
  同一件事只有一种说法，工具描述也短。内部 REST 的形状本来就是可逆的；MCP 侧的变更写进接入文档
- **响应回显规范化后的查询**（`query` 字段，MCP 返回的文本里也有一行），让人和 agent 都看到 yuuka 是怎么理解这句话的。
  不认得的运算符被当成了文本、而它的名字和某个运算符只差一两个字母时，回显里附一句提示：
  `form:alice` 是按文本搜的，是不是想写 `from:`？agent 读到就能自己改正
- **消息列表端点不变**。WebUI 的侧栏视图仍然用结构化参数取列表，不在前端拼查询串。
  store 里列表和搜索共用同一组叶子谓词（现在的 `annotationFilter` 就是这样共用的）
- **不提前收 JSON**。M4 之前没有发 JSON 过滤树的调用方：WebUI 和 agent 都用文本。JMAP 网关来了，JSON 自然就有了入口

#### 文本写法

- 空白分隔的项默认取 AND。`OR` 必须大写，连接相邻的两项，优先级高于隐式 AND（`a b OR c` 是 `a AND (b OR c)`）；
  小写的 `or` 是普通的词。括号分组，`-` 前缀是 NOT
- 一项写成 `名字:值`、而且名字在 16.3 的表里（不区分大小写），才算运算符；否则整项都是普通文本。
  `Re:hello`、`10:30`、`https://example.com` 都按字面搜，不报错。这类词在邮件里很常见，为它们报错只会烦人，
  打错名字的情况靠回显和提示兜住
- 值可以加双引号，引号里是短语，支持 `\"` 和 `\\` 转义；缺了收尾引号的，算到串尾为止。
  JMAP 把单引号也当成短语的界定符，文本写法不这样做，免得 `don't`、`l'été` 被拆坏。JMAP 网关进来的值仍然按 RFC 处理
- **认得的运算符写了不合法的值，就报错并给出位置**（`after:2026-13-01`、`class:foo`）。名字认得，说明意图是明确的，
  静默当成文本只会让人以为过滤生效了。JMAP 定义了、但这一版不支持的条件同样报错（对应 JMAP 的 `unsupportedFilter`），
  它们的名字从第一版起就保留。这样以后加上支持时，已有的查询不会悄悄改变含义
- 上限：`q` 最多 1024 字节、32 个条件、嵌套 8 层，超出报错。查询可能来自被邮件内容诱导过的 agent，所以要有界。
  搜索不提供正则，也就不需要规则那样的正则预算（§8.5）

### 16.3 条件

**JMAP 定义的条件**

| JMAP 条件 | 文本写法 | 这一版 | 匹配什么 |
|---|---|---|---|
| `text` | 裸词、`"短语"` | 支持 | 主题；发件人的名字和地址；To / Cc 的名字和地址；正文；自己发出的信还有 Bcc（见下） |
| `from` | `from:` | 支持 | 发件人的名字或地址 |
| `to` | `to:` | 支持 | To 里的名字或地址 |
| `cc` | `cc:` | 支持 | Cc 里的名字或地址 |
| `bcc` | `bcc:` | 支持 | 自己发出的信的密送地址，取自 outbox；撰写的信还有密送人的名字，取自草稿（见下） |
| `subject` | `subject:` | 支持 | 主题 |
| `body` | `body:` | 支持 | `body_text`（有长度上限，§4） |
| `after` | `after:` | 支持 | `received_at >= 值` |
| `before` | `before:` | 支持 | `received_at < 值` |
| `hasAttachment` | `has:attachment`；`-has:attachment` 是 false | 支持 | `has_attach` |
| `minSize` / `maxSize` | `minsize:` / `maxsize:` | 保留，报 unsupported | 数据在 `raw_objects.size`，暂时没有需求 |
| `header` | `header:` | 保留，报 unsupported | 任意头部只在原件里，没有派生列 |
| `hasKeyword` / `notKeyword` | `keyword:` | 保留，报 unsupported | 已读状态接上 `flags` 以后，`is:unread` 就是 `notKeyword: $seen` |
| `inMailbox` / `inMailboxOtherThan` | 无 | 不做 | Mailbox 的映射留给 M4，见 16.5 |
| `*InThreadHaveKeyword` | 无 | 不做 | 线程级条件，等有了 keyword 和线程列表再说 |

**yuuka 的扩展条件**

| JSON 写法 | 文本写法 | 匹配什么 |
|---|---|---|
| `deliveredTo` | `deliveredto:shop`、`deliveredto:shop@example.com` | 有一条**可见**投递到这个前缀（和域）。精确匹配，按 `FoldAddr` 折叠 |
| `label` | `label:x`、`label:"带 空格"` | 精确匹配，区分大小写（§8.5） |
| `archived` | `is:archived` | |
| `downgraded` | `is:downgraded` | §4.3 的最低降级 |
| `sent` | `is:sent` | 有可见的 outbox 行（§4.6） |
| `class` | `class:machine` / `human` / `unknown` | 生效的分类，人工改判优先 |
| `listId` | `list:` | `list_id`，子串 |
| `triage` | `triage:phishing:yes` | 现有的 `triage=` 过滤，语义不变（§15） |

**`to:` 和 `deliveredto:` 是两回事，界面和工具描述都要讲清楚**：To 是发件人写的，可以伪造，
密送的信和邮件列表的信里根本没有你的地址；投递是 yuuka 自己记下的事实（§9.1）。
要按「我把这个地址给了谁」来查，用 `deliveredto:`。

#### 字符串怎么匹配

RFC 8621 对字符串条件有一套规则，同一套用于 `text` 和每个字符串字段：

- 不区分大小写
- 值按空白切成若干项，**每一项都必须出现**；引号里的短语算一项，要原样连续出现
- RFC 允许整词匹配和词干化（MAY），yuuka 继续用子串：中文不用分词，搜 `invoice` 的一部分也能命中，
  代价是短词的噪声大。这一点和现状一样

JMAP 的 `subject: "foo bar"`（值里不带引号）意思是两个词都要出现在主题里，而不是短语。
文本写法里 `subject:foo` 只带一项，要写短语就用 `subject:"foo bar"`。两条入口最后产出同一种「项」的列表，
匹配只有一处实现。

#### 收件人的名字和 Bcc

两处都按 JMAP 做全：

- **To / Cc 的显示名**。0007 只存了折叠后的地址，显示名留在原件里。现在加两个派生列 `to_names`、`cc_names`
  （解码后的显示名，没有就不存），和地址列一样由 mimeparse 从规范原件读出，`reparse` 会重写它们。
  这两列为 NULL 表示「还没读」，存量信由 backlog（`ingest.ProcessBacklog`）回填，和 0007、0010 的做法一样，
  升级时只需要 `yuuka migrate`。还没回填的信，暂时只能按地址匹配
- **Bcc**。收到的信本来就没有 Bcc。自己发出的信，Sent 副本里也没有 Bcc 头（§10.2.1），所以按 RFC 的字面意思，
  `bcc` 永远不会匹配。yuuka 改用 outbox 里的信封密送地址 `bcc_addrs`，比 RFC 多给了一步。
  outbox 是决定表，不是派生内容，所以 Bcc 不能放进 `messages` 的派生列，也就进不了 trigram 索引。
  `text` 因此多一条分支：`m.id = ANY(ARRAY(子查询))`，子查询找出密送地址命中、而且调用者看得见的 outbox 行对应的信。
  Postgres 把它当作只算一次的 initplan，再和 trigram 索引做 BitmapOr，所以不会让每次搜索都退化成顺序扫描。
  Bcc 的名字只有撰写的信有：撰写（WebUI、REST、MCP）的收件人可以带显示名，Bcc 的名字不写进 Sent 副本，
  留在 outbox 行所来自的草稿上（`drafts.bcc_names`，0014，与 `bcc_addrs` 一一对应）。所以 Bcc 分支的子查询经
  `outbox.draft_id` 连上草稿，按地址或名字匹配；SMTP 提交直接发出的信没有草稿，Bcc 只在信封里（`RCPT TO` 不带名字，
  客户端写的 `Bcc` 头在规范化时就删掉了，§13.6），只能按地址匹配。草稿是决定表，这一支和 outbox 一样不进派生列。
  自己撰写的信 To / Cc 的显示名就在 Sent 副本的头里，`to_names` / `cc_names` 从原件读出时自然收录，不用另做什么

#### 时间

- 值可以是 `YYYY-MM-DD`、RFC 3339 时间戳，或者相对现在的 `after:-7d`，单位只有 `h`、`d`、`w`。
  不提供月和年：`m` 在不同客户端里分别表示分钟或月，有歧义，长跨度直接写日期就行。
  相对时间仍然是 JMAP 的 `after` / `before`，只是值换了一种写法，规范化回显里是算出来的时间点
- 只写日期时，按请求的 `tz` 参数（IANA 时区名）取当天零点，没给就按 UTC。WebUI 带上浏览器的时区，
  MCP 不带，和现在 `since` 的解释一致
- 开闭按 JMAP：`after:2026-10-01 before:2026-11-01` 正好是十月

#### 降级的信默认不出现

和现在一样，搜索默认隐藏降级的信。有两种情况不加这层隐藏：查询里出现了 `is:downgraded`（不论在哪一层、是否被否定），
或者请求参数要求包含（REST 的 `downgraded=include`，MCP 的 `include_downgraded`）。
写了 `is:downgraded` 的人显然想看降级的信，再替他藏起来，这个条件就永远查不到东西。

### 16.4 不变量

- **可见性只在一处算**（§13.8）。每个条件只用调用者本来就看得到的事实：`deliveredto:` 只看可见投递，
  `bcc:`、`is:sent` 和 `text` 的 Bcc 分支只看可见的 outbox 行。**否定也只在可见事实上求值**：`-deliveredto:x` 的意思是
  「没有一条可见投递到 x」，所以结果不会因为一条看不见的投递而改变，也就泄露不了它的存在。
  新条件逐个进 ACL 测试表，命中数和 `total` 都要测
- **条件不能绕过 scope**。Bcc 是信封收件人，outbox 的明细本来就要 `delivery:read`（§13.4）。
  没有这个 scope 时，显式的 `bcc:` 报错，而不是返回空：返回空会让 `-bcc:x` 变成「全部」，调用方也分不清是没有命中还是没有权限。
  `text` 是隐式的，没有这个 scope 时就不带 Bcc 分支。`deliveredto:` 只要 `message:read`，和列表里的 `recipients` 一样
- **索引能用就要用上**。`text` 由 trigram 表达式索引支撑（§4）。字段条件在 SQL 里额外带上一个对整份检索文本的同样的 `ILIKE`：
  字段是检索文本的子串，所以结果不变，而顶层 AND 里的 `subject:`、`from:`、`to:` 也能走索引。
  `OR` 和否定可能用不上索引，个人量级下顺序扫描可以接受，条件数的上限保证它有界
- **检索文本的定义只有一处**。新的检索文本由一个声明为 `IMMUTABLE` 的 SQL 函数拼出：主题、发件人名和地址、
  To / Cc 的地址和名字、正文（`array_to_string` 本身不是 immutable，不能直接写进索引表达式）。
  一个追加的迁移在这个函数上建新索引，再删掉 0002 的 `messages_search`；store 里的表达式和它逐字一致。
  NULL 的列（还没回填的）按空串处理

### 16.5 界面与工具

- **WebUI 的搜索框就是 `q`**，URL 里只有这一个来源。在标签或前缀视图里聚焦搜索框时，预填 `label:"x" ` 或
  `deliveredto:x@zone `；在「已归档」里预填 `is:archived `，在「降级」里预填 `is:downgraded `。
  会话、通知、全部三个视图不预填，搜索仍然跨两区（§12.9）。范围写在框里，人看得见，也删得掉，
  而不是藏在视图状态里
- 搜索框旁边放一份运算符说明。`label:` 和 `deliveredto:` 的值从已经加载的标签和已认领前缀里补全。
  回显的规范化查询和输入不一致时（比如有一项被当成了文本），在框下面用系统声音写一行小字说明（§12.5）
- **MCP**：`search_messages` 的工具描述用一两行列出运算符，并点明 `to:` 和 `deliveredto:` 的区别。描述别写长：
  每个 agent 的工具列表都要为它付 token（§8.7）
- **JMAP 的 Mailbox 映射留给 M4**，这里只保证映射得过去。RFC 8621 允许一封信同时在多个 Mailbox 里，
  就是为了照顾标签式的系统，所以标签可以映射成 Mailbox；归档、发件、垃圾三个 role 分别对应 `archived`、`sent`、`downgraded`。
  标签**不映射成 keyword**：keyword 不区分大小写、只许 ASCII，而 yuuka 的标签区分大小写、可以写中文。
  为了 JMAP 收紧标签规则，代价落在每天用标签的人身上，而 JMAP 客户端本来就少（§5）
- 结果仍然是一封一行，按 `received_at` 倒序，用键集游标分页（§5.1）。按线程聚合对应 JMAP 的 `collapseThreads`，
  随会话区的线程列表一起做

### 16.6 放弃了什么

- **Gmail 完全兼容**。Gmail 和 JMAP 冲突的地方（日期写法、`to:` 的范围、标签的大小写），以 JMAP 为准
- **只收 JSON 过滤树，不做文本写法**。agent 写 JSON 没问题，人在搜索框里写不出来。文本写法只是一层薄皮，底下仍然是同一棵树
- **保留 REST / MCP 上的结构化参数**。它们和 `q` 里的运算符说的是同一件事，留着就是两种说法、两套校验，
  工具描述还更长。回显和报错已经能把 agent 引到正确的写法
- **复用规则的匹配树**（§8.5）。规则在 Go 里对一封信的事实求值，并且明确不编译成 SQL；
  搜索必须在 SQL 里算 `total` 和分页。两边的字段也不一样：规则有 `from.verified`、`from.org`，要用 publicsuffix 在 Go 里判；
  搜索有标签、时间、自己发出的信。用同一套名字表示两种语义，迟早有人会照着搜索结果写出一条漏信的规则。
  「从搜索生成规则草稿」以后可以做，但只翻译两边语义完全相同的条件，命中数由规则求值器自己算（`CountMatches`），
  有差异就直接显示出来，不指望两边各自保持一致
- **CEL、CUE 一类的表达式语言**。人的输入习惯是邮件客户端那一套，M4 要对接的协议是 JMAP，
  再加一门语言只是多一样要学的东西
- **正则**。成本要算，查询还可能来自被诱导过的 agent；需要正则的场景就写规则

### 16.7 分期

1. 解析器、规范化回显和改写提示、Filter 编译；REST 和 MCP 的搜索改用它，去掉旧参数，同步改接入文档
2. 补齐 JMAP：`to_names` / `cc_names` 两个派生列和回填，新的检索文本函数和索引，`text` 的 Bcc 分支
3. WebUI：按视图预填、运算符说明、值补全、回显提示
4. 以后再做：`is:unread`（等已读状态接上）、`has:` 按提取值过滤（验证码、链接，§12.9）、`minsize:` / `maxsize:`、
   命中高亮、从搜索生成规则草稿

第 1 步不依赖第 2 步：`to:` / `cc:` 先只匹配地址，`text` 先不含 To / Cc 和 Bcc，第 2 步落地后自动补全，查询的写法不用改。
不提前做 `is:unread` 和提取值过滤：前者没有数据，后者等通知区真有「按码找信」的需求再加，加的时候不改已有查询的含义。
