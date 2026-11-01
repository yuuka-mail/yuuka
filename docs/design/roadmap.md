# 分期路线与已知风险

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/design/README.md](README.md)。

## 6. 分期路线

| 阶段 | 内容 | 产出 |
|---|---|---|
| **M0** | Worker（pending 标记 + R2 + HMAC webhook + 写入截止时间，§2.1）；**Worker ↔ Go 契约与共享测试向量**（§2.2）；**最小 Postgres**：只建 `raw_objects` + `deliveries`；`content_hash` + 以唯一约束实现的重投去重（§4.1）；Go ingest + raw 归档 + 幂等 + reconcile；TG 推送一行摘要 | 一周内可日用：所有信已自持 + 有通知。无 UI |
| **M1** | go-message 解析入库；**主体/凭据建表**（§13.9）；**`changes` 变更日志建表**（§4）；前缀 claimed/discovered 两态 + 最低垃圾降级（§4.3）；WebUI 内置 passkey 登录（§13.7）+ 列表/阅读/按前缀聚合；trgm 搜索；**agent 读路径**（文本化视图 + MCP 只读 tools） | 替代现有查看方式；agent 已能检索邮箱 |
| **M2** | **REST 契约迁到 OpenAPI**（§5.1，先于写端点）；outbox 状态机（含 `unknown` / 逐收件人结果）+ 任意前缀发信 + 回复/线程；**CF 出口的字节比对实测**（2026-10-31 已测：Message-ID 从最终回复回填，§10.4.1）；草稿可由 agent 起、人确认发出；**SMTP submission 口**（§13.6，隐式 TLS + PROXY protocol），其他服务可直接发信；`accounts` / `identities` 建表（§9.5） | 不再用 curl 发信 |
| **M3** | **界面 i18n**（§12.11，先于本阶段的界面文案）；规则引擎 + machine/human 分区 + 验证码提取 + 前缀注册表/泄漏检测；决策模型分诊（§15，可选，默认影子模式）；agent 提案→规则的闭环；**`changes` 游标的拉取**（§8.6，SSE 推送仍在 M4）；**agent 专属身份的 `review` / `auto` 外发**（§8.9，外泄检查依赖本阶段的验证码提取）；管理动作进 WebUI（§13.10，放宽权限仍只在 CLI） | 本项目的差异化能力 |
| **M3.5** | **§17 已先落地**：出口、身份的出口链、通知渠道与绑定进库，第三方机密做信封加密（拉取账户的密码要有地方存）。多账户：只读 IMAP 拉取（先接 Gmail，app password，`sync_policy=full`；自己写的最小客户端，§5）+ 跨账户去重 / 线程归并 + 回复从收信账户的身份和出口走，不串身份；`account_access` 生效（`default_read` 加行覆盖，§13.8）；`offload_policy` / `remote_state` 建列不写（§9.6）；`metadata` 策略是最后一步，可单独砍。分步见下表 | 一个收件箱看全部 |
| **M3.5b** | OAuth：Gmail OAuth、M365，`oauth_clients`，来源和出口的 `xoauth2`，出口的 STARTTLS，access token 的主机名单（§17.2、§17.8） | 需要 OAuth 的邮箱也能接 |
| **M3.6** | 归档卸载（§9.6）：拉取稳定一个周期后，按账户开 `offload_policy`，释放外部邮箱容量 | 容量受限的邮箱不再需要清理 |
| **M4** | JMAP 子集 + 自己的客户端/PWA；SSE 事件流（人和 agent 共用增量游标） | 多端 |
| **M5（可选）** | go-imap 网关；入口切换到自建 MX 的 driver | 去 Cloudflare 依赖 |

**M3.5 的分步**（P1–P6 已合入；P0 的实测和 P7 未做）：

| 步 | 内容 | 依赖 | 触及的单向门 |
|---|---|---|---|
| P0（设计已合入，实测未做） | 设计：§2.2、§4、§5、§9、§10.1、§13.8、§17.8 和本表；用真实账户做 §10.1.5 的实测 | — | 文档本身不是，但它定下 `pull/`、`source_key` 的写法和可见性的默认 |
| P1（已合入） | 迁移（[0019](../../internal/store/migrations/0019_pull_accounts.sql)）和存储：`CommitPull`、游标 CAS、`pull_failures`、来源的配置与停用（机密 CAS 照出口）、`default_read` 并**打开过滤**（单独一个提交，带升级前后的对照测试）、`TestVisibility` 加拉取账户、锁顺序测试加账户锁 | P0 | 迁移、可见性 |
| P2（已合入） | `internal/imap`：词法、命令、SASL `PLAIN`、literal 上限、modified UTF-7；fuzz；`imapserver` 对手和录音 golden | P0 | 否 |
| P3（已合入） | `pull/` 的 key：核对函数、带 `yuuka-pull` 的条件写、测试替身、本地副本接受 `pull/` | P0 | Go 对 raw bucket 的写（§2.2） |
| P4（已合入） | 同步引擎（§10.1.3、§10.1.4）：管理协程、每账户 worker、advisory lock、一轮的全部步骤、回填、预算、退避、登录被拒即暂停、只有头部、失败表、每周对账、UIDVALIDITY 重映射；`fold` 和 reparse 按账户取可信接收方；推送和评分的抑制 | P1、P2、P3 | 不丢信 |
| P5（已合入） | `internal/manage` 与 CLI `yuuka account …`、`serve` 接线、运维文档（含 `YUUKA_TRIAGE_ACCOUNTS`） | P1；`test` 要 P2 | CLI 名字 |
| P6（已合入） | REST、WebUI（设置页的外部邮箱、读信时标出账户）、MCP `list_accounts`、`account:` 搜索、agent 视图带账户、`TestACL`、接入文档、截图 | P1、P5 | 新增读路径进 ACL 表 |
| P7（可砍） | `metadata` 策略：只拉头部、`FetchBody` 的定位验证、取正文记 `actions`、`no-store`、agent 取正文要授权 | P4、P6 | 可见性（新读路径） |

P1、P2、P3 可以同时开工；P4 是汇合点；P5 和 P4 可以并行，`cmd/yuuka` 的接线按 P4 → P5 的顺序合。

M0 刻意不碰解析：**先保证信不丢、能通知**，剩下全是可重放的派生物。
Postgres 在 M0 只建两张表：§2.1 的 Go 侧提交点是 `raw_objects` + `deliveries` 的一个事务，
§4.1 的重投去重也落在这两张表上——`content_hash` 是原件的字节属性，存在 `raw_objects` 上，
只需要切分头部、不需要 MIME 解析；重投靠 `deliveries.source_key` 的唯一约束撞键。
所以 M0 不依赖 M1 的 `messages`，M1 加上 `messages` 时也不必回头改这两张表。
它们 M1 本来就要，提前建不会产生要扔掉的代码；
换成 M0 专用的文件或 SQLite 索引，反而要把去重写两遍、再迁移一次。

---

---

## 7. 已知风险和取舍

- **常驻机器是运维者自己的运维责任**：备份、证书、升级。缓解：R2 里有不可变原件，
  邮件内容本身重建只需重跑 reparse。但 **Postgres 不只是派生物**——前缀标注、规则、提案、
  审计、主体与凭据、草稿都无法重建（§4.2），要做正经的 `pg_dump` / WAL 备份；
  备份里含凭据哈希，§17 落地后还有第三方机密的密文（§17.3），按敏感数据保管；
  包装 DEK 的 KEK 不进备份，单独离线保存——它丢了要重新输入所有第三方机密，但不丢信。
- **catch-all 的垃圾量是一等运维问题**：字典攻击会把前缀注册表和通知通道淹没。
  缓解：前缀两态 + 最低垃圾降级从 M1 就有（§3.3、§4.3），内容判定由 M3 的决策模型分诊补上（§15）。
- **Worker 是 TS，不是 Go**：接受。代价是 ~150 行必须手工审读的胶水代码，
  换来不用自己养 MX 和 IP 信誉。
- **Email Sending 仍是 Beta**，要 Workers Paid；含 3000 封/月，超出 $0.35/1k；
  单封 ≤25 MiB、≤50 收件人；日配额按账号信誉动态放开。入口 driver 抽象就是
  为了它哪天变脸时能换。
- **Email Routing 入站 ≤25 MiB**，超了在 CF 侧就被拒，本系统完全看不到。
  可以在 DNS/MX 层面接受这个事实，或 M5 自建 MX 时解决。
- **中文检索**：见 §4，先凑合，留替换口。
- **双向同步是最容易出 bug 的地方**：外部账户默认**单向拉取**，回写（已读/归档/标签）
  按账户开关，冲突 last-writer-wins，本地 flag 独立一层永不被覆盖。
- **归档卸载（§9.6）让 yuuka 成为外部邮件的唯一副本**：这是整份设计里唯一不可逆的远端动作。
  一旦某账户开了卸载，"原件在 R2 可重放"就不再是兜底而是**全部**——R2 + 本地双副本、
  二次校验、备份快照先行，三道闸门缺一不删；备份策略从"该做"变成"不做就是丢信"。
- **受合规约束的账户不该全量落到个人机器**：`sync_policy=metadata` 或 `notify`，
  正文按需拉取不留存；这是合规问题，默认取保守值。
- **M365 的 OAuth token 要续期**（Gmail 默认 app password，§5），拉取要限速；
  `uidvalidity` 变化要能全量重扫。
- **Gmail 的 app password 可能被单方面关掉**：Google 一直在收紧「不够安全的应用」，Workspace 管理员也能禁用它。
  那时这个账户只能等 M3.5b 的 OAuth；远端还保有信，是延迟而不是丢信。
- **IMAP 客户端是自己写的**（§5）：会在第一台怪服务器上出错。错误只会让一轮失败、游标不动，不会丢信；
  反复取不下来的信进失败表，看得见（§10.1.3）。
- **回填期间 `changes` 会有一大波事件**，订阅方会收到；和 0007 的补跑是同一个先例。
- **分类必然有误判**：所以 UI 里"改判"是一等操作，且改判产生规则。

---
