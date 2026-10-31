# 现有方案对比

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 1. 为什么不直接用现成项目

Cloudflare 生态里已有若干成熟的邮件项目。先说结论：**它们都能用，只是目标不同**——
多数项目的核心用例是「临时邮箱」或「AI 邮件客户端」，而本项目的核心用例是
**把多个 catch-all 域当主力邮箱长期自持**。下面的「不适用」全部是用例错配，
不是质量评价。

| 项目 | 定位 | 对本用例的错配 |
|---|---|---|
| [maillab/cloud-mail](https://github.com/maillab/cloud-mail) | 功能完整的自建邮箱，Vue + D1 + R2，自带 Telegram 推送 | 地址需显式创建；无原件留存层；无 agent 面 |
| [cloudflare/agentic-inbox](https://github.com/cloudflare/agentic-inbox) | 官方的 AI 邮件客户端示例，Durable Objects + R2 | 见 §1.1 |
| [dreamhunter2333/cloudflare_temp_email](https://github.com/dreamhunter2333/cloudflare_temp_email) | 临时邮箱，功能面最广（TG bot、webhook、SMTP/IMAP 代理） | 数据模型围绕「一次性地址」，与长期自持相反 |
| [Newspicel/cloudflare-mail](https://github.com/Newspicel/cloudflare-mail) | Worker 内实现 IMAP4rev1，另有 iOS 客户端 | IMAP 依赖 Workers inbound TCP 私测 + Spectrum，不可依赖 |

### 1.1 agentic-inbox 的四处错配

官方示例的 ingest 链路（`workers/index.ts` 的 `receiveEmail`）和数据模型
（`workers/db/schema.ts`）值得细读，本设计的若干取舍正是读完它之后才定下来的。
它作为「AI 邮件客户端」的参考实现很合适，但有四处与 catch-all 主力邮箱的用例直接冲突：

| agentic-inbox 的做法 | 与本用例的冲突 | 本设计的做法 |
|---|---|---|
| 解析完 MIME 只存 `body = html \|\| text` 和 `raw_headers` JSON，**不保留原始 MIME** | 不可重放：解析器升级、验签、导出 `.eml`、给客户端完整 MIME 全都做不到 | 原始 MIME 不可变落盘，先存后解析，派生数据随时可重建（§3.1） |
| 每个地址一个 Durable Object + 独立 SQLite | 对 catch-all 不是合适的拓扑：无法跨前缀全局搜索与聚合，前缀一多就是几百个库 | 单库，**`(域, 前缀)` 是维度不是分库**（§3.2） |
| 收到未注册地址的信直接 `console.log("Ignoring email")` 丢弃 | 与 catch-all 的定义相悖——catch-all 的全部意义就是接住没预先登记的前缀 | 不丢信原则：未知前缀照收并自动登记（§3.5） |
| `date` 存接收时间而非 `Date` 头；发信 `waitUntil` fire-and-forget，失败只打日志 | 排序错乱；发信静默失败 | 双时间戳；出口走状态机 + 重试 + 审计（§3.6） |

反过来，以下做法本设计直接沿用：

- `References[0] || In-Reply-To || self` 的线程归并，外加无线程头时按 `subject + sender` 兜底匹配
- 发信前先写 Sent 副本，再交给 transport
- 内置 MCP 端点，让外部工具直接操作邮箱

不沿用的一点：它把鉴权完全交给 Cloudflare Access。本设计由应用自己认证每个端点，
前置身份代理只作纵深防御（§13.7）。
