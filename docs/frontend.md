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
