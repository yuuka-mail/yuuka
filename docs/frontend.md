# 前端技术选型

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 12. 前端技术选型

选型沿用既有项目的栈：React 19 + Vite + TS + Tailwind 4 + shadcn + TanStack Query。
理由三条：

1. **yuuka 和 arisu 将来是并肩使用的两个界面**（§11.1 的人格分层），
   同一套设计语言和组件才能让"系统在说"和"agent 在说"用同一组视觉 token 表达
2. 既有项目多数落在这个栈上，熟练度与组件复用度最高；单人长期维护的项目里这条权重最大
3. shadcn 是 copy-in 不是依赖——组件进自己仓库，十年项目不会被上游牵着走

```
React 19 + Vite + TypeScript + Tailwind 4 + shadcn/ui + TanStack Query
+ TanStack Virtual（虚拟列表）+ vite-plugin-pwa
```

### 12.0 Next 还是 Vite：选 Vite

Next 和 Vite 在既有项目里都在用，**所以这里没有熟悉度成本**，
可以纯按项目特性定。按特性定的结果是 Vite：

- **要 `go:embed` 进单个二进制**。Next 的价值集中在 SSR / RSC / server actions /
  middleware / image optimization，而 `output: 'export'` 会把这些全部关掉——
  等于付了 Next 的复杂度却拿不到它的好处
- **登录后才有内容，没有 SEO 和首屏需求**。SSR 在这个项目里不解决任何问题
- **依赖面和构建时间**。十年尺度的自用项目，上游升级噪音是真实成本，
  Next 的 major 升级历来比 Vite 吵

反过来说，什么情况该回头选 Next：如果哪天 WebUI 不再 embed 而是独立部署
（比如挂到 Cloudflare Pages 上，给手机当入口），那 Next 的价值就回来了。
但那和 §3.1 的"单二进制、数据在自己机器上"是相反的方向，暂时不考虑。

shadcn 对 Vite 的支持没有问题（它本来就是 copy-in 的组件源码，不绑框架脚手架），
既有项目里已有 Vite + shadcn 的组合，配置可直接沿用。

### 12.1 砍掉的两块复杂度

**一、不引富文本编辑器，撰写用 Markdown。**
ProseMirror / Lexical / TipTap 任意一个都会带进一整套生态和长期升级义务。
而开发者自用邮箱里 Markdown 更顺手，引用回复用 `>` 天然贴合邮件传统。
发送时在 Go 侧生成 `text/plain` + `text/html` 两份（§10.2 本来就是我们自己组装 MIME，
具体规则见 §10.2.1）。

编辑器定 **CodeMirror 6**（模块化、框架无关）：要的是 Markdown 高亮、软换行、
`>` 引用的自动续行、粘贴图片转附件这几件事，textarea 全都得自己补。
M2 第一周用 textarea 占位可以，但别让粘贴和快捷键逻辑长在 textarea 上，
否则迁移时要重写。

**二、通知区不渲染原始 HTML，直接复用 agent 的结构化视图。**
§8.1 已经为 agent 做了 `text/agent`：去样式、去 tracking pixel、
保留结构、带稳定锚点。通知区要的恰好就是"一眼看到码"，
**所以它和 agent 消费同一份数据**——省掉一整套渲染逻辑，而且人和 agent
看到的内容天然一致，不会出现"agent 说有码但界面上没有"。

### 12.2 邮件正文的两条渲染路径

| 区 | 渲染方式 | 远程资源 | 锚点 |
|---|---|---|---|
| 通知区（机器邮件） | 结构化视图（§8.1）直接渲染成 DOM | 不加载 | ✓ 可定位 |
| 会话区（人类邮件） | `<iframe sandbox srcdoc>` + CSP | 默认阻断，点击才经自己的代理放行 | ✗ |

**重要约束：`yuuka://message/<id>#part/2/para/7` 只在结构化视图上成立。**
iframe 里是原信自己的 HTML，我们无法在其中挂稳定 id。所以 agent 的引用
（§8.5 的 `cites`）默认指向结构化视图；人想看原样时再切到 iframe。
这一条必须在实现前说清，否则锚点会在会话区悄悄失效。

`sandbox` 不要加 `allow-scripts`。邮件 HTML 永远是不可信输入（§8.3），
而 catch-all 意味着任何人都能投递它。

附件是第三条路径，而且不渲染：一律作为下载返回（`Content-Disposition: attachment`、`nosniff`、
`sandbox` CSP），不做预览。附件里的 HTML、SVG、PDF 都可能带脚本，预览器就是新的攻击面；
需要预览时再单独设计（M1 的落法见 §12.7）。

### 12.3 几个必选的单点

| 需求 | 选择 | 理由 |
|---|---|---|
| 列表 | **TanStack Virtual** | catch-all 的机器邮件是几万条量级，虚拟滚动不是优化而是前提 |
| 服务器状态 | **TanStack Query** | 缓存失效/重试这些坑别自己踩 |
| 实时增量 | 原生 `EventSource` + `since_state` 游标（§8.6） | 收到事件后 invalidate 对应 query key，不全量重拉 |
| 类型同源 | **`tygo`**（Go struct → TS）挂 `go:generate` | 手写 TS 类型必然和 Go 漂移，漂移在邮件这种字段多的模型里尤其痛 |
| 产物 | `go:embed web/dist` | dev 时 Vite dev server，API 走 Vite proxy 到 Go |

组件库的边界要认清：邮件客户端 UI 高度特化（三栏布局、虚拟列表、线程视图、
前缀聚合），**shadcn 只能提供基础件**（Dialog / Command / Dropdown / Toast），
列表区和阅读区都得自己写。不要指望组件库解决布局。

### 12.4 PWA 顺带解决"本地客户端"

`vite-plugin-pwa` + IndexedDB 缓存最近 N 封 → **WebUI 装到手机上即是自有客户端**。
这直接影响路线图：§6 的 M4 里"自己的客户端"不需要另写，
**JMAP 的优先级因此下降**——它从"我自己客户端的前提"退回成"对外协议"
（给第三方工具、给将来的 IMAP 网关用）。

一个要贯穿下来的策略：浏览器缓存是**又一份数据副本**，
所以 `sync_policy=metadata` 的工作邮箱（§9.2）不该缓存到 IndexedDB。
账户级策略必须一路传到前端缓存层，不能只在后端生效。

### 12.5 agent 面板的视觉分层

§11.1 要求"系统在说"和"agent 在说"一眼可分。实现上这是**两套设计 token**
（颜色、图标、署名位），不是两种文案口吻：

- `system` / yuuka：事实陈述——新信到达、出口失败、配额告警、正则提取的验证码
- `agent:arisu`：推测与提案——摘要、分类建议、起草的回复，**必须带 `cites` 锚点**

提案卡片就地长在邮件旁（而不是另开聊天框），接受/拒绝是一等按钮，
接受后可勾选"以后同类自动处理"→ 落成 `rules`（§8.5）。

### 12.6 前端的分期

- **M1**：三栏骨架 + 虚拟列表 + 前缀聚合 + 结构化阅读 + 搜索。
  **不做** iframe 渲染，**不做**撰写器——会话区要等 M2 的发信能力
- **M2**：iframe 阅读 + Markdown 撰写 + 回复引用 + 身份选择器（默认取自 delivery）
- **M3**：提案卡片 + 规则编辑 + 前缀泄漏视图
- **M4**：PWA + IndexedDB 离线

### 12.7 M1 的落法

M1 的 WebUI（`web/`）和它读的 REST（`internal/api`），以及设计里没写死、实现时定下的几处：

- **REST 只读、按路由声明 scope**。`/api/v1/` 下七个端点：`messages`（列表，过滤 prefix / class /
  降级，`(received_at, id)` 键集游标）、`messages/{id}`、`messages/{id}/view`（§8.1 的结构化视图，
  `level=snippet|sections|part`，默认 `part`；`anchor=part/<section>[/para/<n>]`，不带 `#`）、
  `messages/{id}/attachments/{part}`（附件下载，见下）、`threads/{id}`、`prefixes`（聚合计数）、`search`。`prefixes` 要 `prefix:read`，其余要 `message:read`；
  `messages/{id}` 里的投递事实（信封、认证结果）另要 `delivery:read`，没有就不给，
  `view` 里的 `facts.delivered_to` 和 `facts.auth` 同理（标 `withheld`，§8.1）。路由和 scope 写在一张表里，
  ACL 测试遍历这张表，漏一条测试就失败。列表用键集而不是 offset，是因为新信到达时 offset 分页会重复或跳行
- **附件只下载，不渲染**（§8.3、§12.2）。`messages/{id}/attachments/{part}` 的 `{part}` 是结构化视图
  `content.attachments` 里报的 IMAP section 号；要 `message:read`，经 `MessageByID` 按 Viewer 取信、
  原件只从可见的投递读（§13.8）。什么算附件只由解析器的 `Attachment` 判定决定，和列表同源：
  正文、`multipart` 容器、以 Content-ID 内嵌在 HTML 里的图片一律 404。字节只解传输编码（base64 / QP），
  不做字符集转换——下载的是发件人附上的原样文件；base64 解不开就 422，而不是给一个悄悄变短的文件。
  响应头固定：`Content-Disposition: attachment`，文件名按 RFC 6266 给两份（ASCII 回退的 `filename=`
  和 RFC 8187 百分号编码的 `filename*=UTF-8''`），先去掉路径、控制字符和双向格式字符（防 `txt.exe`
  显示成 `exe.txt`）、开头的点，限 200 字节且保留扩展名；`X-Content-Type-Options: nosniff`、
  `Content-Security-Policy: default-src 'none'; sandbox`、`Cross-Origin-Resource-Policy: same-origin`、
  `Cache-Control: no-store`。`Content-Type` 只在声明类型属于 `application/pdf`、`image/png`、
  `image/jpeg`、`image/gif`、`image/webp` 时照用，其余一律 `application/octet-stream`：
  反正不会内联显示，类型只影响保存下来的文件被系统认成什么；这几种是最常见、打开时不执行脚本的格式，
  HTML、SVG、XML、脚本和 `text/*`（声明的字符集可能是错的）都不在内。
  每次下载记一条 `actions`（`api.attachment.download`：谁、哪封、哪个 part、字节数；不记内容和文件名），
  记不下来就不给下载。agent 侧还没有对应的 MCP 工具，`get_message` 里附件仍标「此处不可读」
- **类型由 `tygo` 生成**，覆盖 `internal/api/types.go` 和 `internal/authn/wire.go`
  （`/auth/` 的响应体）以及 `internal/agentview/types.go`（结构化视图），`mise run web:check` 先 `go generate` 再类型检查，Go 侧改了字段 TS 立刻报错
- **页面壳不需要会话**。`go:embed` 的 `index.html` 对任何非文件路径都返回（`no-store`），
  哈希命名的 `assets/` 缓存一年；壳加载后再同源 `fetch /auth/session`，401 就原地显示登录，
  URL 不变，所以从 TG 链接点进来、登录后直接落在那封信上。视图状态全部在 query string 里
  （`prefix` `class` `dg` `q` `m`，以及 `a`：打开的信里的锚点），不引路由库
- **`web/dist/` 只跟踪 `.gitkeep`**，让新克隆不先构建前端也能编译 Go；这样的二进制对页面请求回 503
  并提示 `mise run web:build`。代价是 Vite 不能清空输出目录，改为构建前只删 `dist/assets`
- **CSP** 是 `default-src 'none'` 加 self 的脚本、样式、图片、连接，`frame-ancestors 'none'`。
  M2 加 iframe 时在这里开 `frame-src`
- **登录页不链接恢复入口**（§13.7：恢复码不出现在日常登录路径上）；`/recover` 换到令牌后直接跳
  `/enroll#<token>`。`/enroll` 读完片段立刻从地址栏抹掉；没有令牌但有会话时，它变成「再加一把 passkey」
- **系统声音与 agent 声音**（§12.5）落成 `--system-*` / `--agent-*` 两组 token 和一个
  `Voice` 组件：颜色、标记形状（方 / 圆）、署名都不同。M1 只有系统在说：阅读区的认证结果、警告、
  剥掉了什么、接收时间是 yuuka 的事实陈述（结构化视图的 `facts`），与信头自称的 `Date` 分开显示。
  `dmarc=fail` 和隔离是错误色、带边框；未验证和降级是警告色，都排在事实框最上面
- **阅读区渲染结构化视图**（§12.1），和 agent 的 `get_message` 是同一棵树：信头和线程条仍取
  `messages/{id}`，正文和事实取 `messages/{id}/view?level=part`，按 `more` 分段续取
  （`web/src/mail/reading-data.ts`），`reading-body.tsx` 把块渲染成 DOM，块的锚点就是 DOM id。
  `?m=<id>&a=part/1/para/3` 打开时滚到那一块并高亮，不在第一段里就自动续取到它出现为止；
  悬停块左侧的 ¶ 把锚点写进地址栏并复制 `yuuka://` URI。正文一律是文本节点，不用
  `dangerouslySetInnerHTML`；链接 `rel="noopener noreferrer"`、不带 referrer，旁边显示真实主机名
  （punycode 原样显示），链接文字自称的主机和真实主机对不上时标成错误色。附件只列出，不链接。
  取舍：引用折叠后只剩一行标记，看不到被引的原文——那是线程里的另一封信，点线程条去看
- **shadcn 选 Base UI 作底层**（`components.json` 的 `base-nova`），只拷了 Button / Input / Badge /
  ToggleGroup / Skeleton / Separator；三栏、列表、阅读区按 §12.3 自己写
- **lint 用 oxlint 而不是 ESLint**：仓库固定 TypeScript 7（与 Worker 一致），typescript-eslint
  还不支持它；oxlint 不依赖 TS 的 JS API，一个依赖就带齐 React hooks 规则。typescript-eslint
  支持 TS 7 之后可以换回去，规则集不变
- **开发**：`mise run web:dev` 在 `localhost:31473` 起 Vite，把 `/api`、`/auth` 代理到
  `YUUKA_DEV_API`（默认 `127.0.0.1:31480`）；那个 `yuuka serve` 的 `YUUKA_WEB_ORIGIN` 要设成
  `http://localhost:31473`，WebAuthn 和跨源检查才认。`go run ./internal/devseed/cmd` 把合成语料
  和生成的变体灌进开发库，原件写进 `YUUKA_DATA_DIR`（或 `-data-dir`），结构化视图从那里读；`pnpm run screenshots`（`web/scripts/screenshots.mjs`）用 Chrome 的虚拟
  认证器走完注册和登录，截亮、暗、窄屏三套图
