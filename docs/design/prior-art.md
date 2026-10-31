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
| [cristianadrielbraun/gofer](https://github.com/cristianadrielbraun/gofer) | 自托管 webmail，Go + SQLite，聚合 IMAP / Gmail / Outlook / JMAP 账户，带日历、通讯录 | 不在 Cloudflare 生态里，和 M3.5 的多账户聚合重叠；本地库是远端的缓存，不是权威存储。见 §1.2 |

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

### 1.2 gofer：同样聚合外部账户，但本地只是缓存

gofer 和本项目在「一个收件箱看全部」（§9）上重叠，下面基于 2026-10 的 v0.19.0 源码。
**根本差别在本地数据的地位**：gofer 把远端当权威，本地 SQLite 只是缓存。远端删了的信本地就删，
UIDVALIDITY 一变就清掉该文件夹的状态重拉；同步只取 ENVELOPE 和几个线程头，原件点开时才下载，
超过 64 MB 不下载。这对客户端是对的取舍，对本项目不成立：yuuka 是原件的权威存储（§3.1），
卸载之后还是唯一副本（§9.6），远端消失只能记成投递的状态，不能删行。
它没有 agent 面，也不收 catch-all。

它的迁移历史（v0.19.0 时 schema 已到 133 版）记录了几次改方向，其中几次本设计一开始就绕开了：

| gofer 后来改掉的 | 本设计 |
|---|---|
| 每个账户按 Message-ID 一行，发给自己的信在 Sent 和 Inbox 的两份副本被合成一行，后来把远端 id 挪到「文件夹状态」上，又补了副本表 | 内容 / 投递两层从第一天就拆开，折叠键是内容哈希（§4.1） |
| IMAP ENVELOPE 给的 Message-ID 不带尖括号、本地 Sent 副本带，于是重复入库；缺 Message-ID 时合成 `<文件夹-UID@…>`，同一封信在两个文件夹里是两行，UIDVALIDITY 一变又换一个 | 不拿 Message-ID 做身份，缺失时也不合成；跨文件夹的身份交给 `X-GM-MSGID` / `EMAILID`（§10.1） |
| 凭据密文的 AAD 先用 NUL 拼接字段，有歧义，改成带长度前缀的第二版并启动时重写所有行 | 落地时直接用带长度前缀的版本（§13.3） |
| 从共享库改成每个用户一个库，写了一整套带日志、可续跑、单向的转换 | 单库，前缀和账户都是维度，不是分库（§3.2、§9.5） |

结论和本设计一致、可作旁证的：

- **发信崩溃后的处理**：启动时把所有「发送中」改成「可能已发出」，重发要人确认。
  本设计以提交点区分 `abandoned` 和 `unknown`（§4.5），粒度更细
- **认证头不可信**：展示时它只认最上面一条 `Authentication-Results`，做安全决定时自己验 DKIM 并要求与 From 域对齐。
  本设计只认可信接收方的那一条（§10.1.1）
- **规则在入库的同一个事务里排队、动作幂等，并且记下「规则做了什么」**；默认不匹配已发送和草稿。
  本设计在 `Fold` 事务里求值，每条改了东西的规则写一行 `actions`（§8.5）

真正值得拿来的有四处：它在 IMAP 和 Gmail 上踩过的坑（§10.1.2），远程图片代理的出站防护（§12.2），
凭据密文的绑定方式（§13.3），以及把不受 ACL 约束的 store 方法列成清单（§13.8）。
不学的是它的功能面扩张速度（日历、通讯录、翻译、三套主题，一个月五百多个提交，CI 只构建镜像不跑测试），
以及在 iframe 里加 `allow-scripts` 跑自己的脚本：本设计不加（§12.2）。
