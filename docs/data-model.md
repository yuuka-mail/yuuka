# 数据模型

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 4. 数据模型（Postgres，先给核心表）

一封信在模型里拆成**内容**和**投递**两层——因为同一封信可能同时落到
`a@example.com`、`b@另一个域` 和 Gmail（CC 或多地址投递），内容只该存一份：

主体、凭据与 identity 授权三张表见 §13.2–§13.5，此处不重复。

```sql
-- 账户 / 身份 / 传输（多账户整合的地基，见 §9）
accounts(id, kind, label, auth_ref, sync_policy, writeback_policy, offload_policy, active)
  -- kind: cf_catchall | imap | gmail_api | smtp_only
  -- sync_policy: full(存原件) | metadata(只索引,正文按需) | notify(只通知)
  -- offload_policy: off | after(...)，归档卸载（§9.6），仅 sync_policy=full 可开
identities(id, address|pattern, zone, display_name, signature, transport_id, is_default,
           owner_id → principals)   -- 身份归属：决定 agent 外发档位的硬上限（§8.9）
transports(id, kind, config_ref)   -- cf_smtp | gmail_smtp | m365 | ...
zones(zone pk, account_id, catchall bool, sending_onboarded bool)

-- 原始件（不可变）
raw_objects(id ulid pk, r2_key, local_path, size, sha256 unique, received_at, ingest_state,
            completeness smallint)  -- full | header_only（§4.4）；原件不可变，补全正文是另一份 raw

-- 消息（派生，可重建；id 跨 reparse 稳定，见 §4.2）
messages(
  id ulid pk,
  canonical_raw_id → raw_objects,  -- 展示/导出用的那份原件；每条投递各有自己的 raw（见 deliveries）
  message_id text,                 -- 原信 Message-ID；缺失时合成 <sha256@zone>
  body_hash bytea,                 -- 去传输头后的原件哈希；header_only 时为空（§4.4）
  unique(message_id, body_hash),   -- 折叠键，单独 message_id 不够，见 §4.1。body_hash 为空的不参与折叠
  in_reply_to text, refs text[],
  thread_id ulid → threads,
  date_header timestamptz,          -- 原信 Date
  received_at timestamptz,          -- 实际到货，排序用这个
  subject text, snippet text,
  from_addr text, from_name text,
  class smallint,                   -- 分类器 + 规则算出的 machine / human / unknown；人工改判不写这里
  spam_score real,                  -- 来源见 §4.3
  has_attach bool, body_text text, body_html_key text,
  parser_version int                -- 哪个版本的解析器产出的这一行；段落锚点随它漂移（§4.2）
)

-- 人对一封信做的决定：不可重建，reparse 不碰
message_state(
  message_id → messages pk,
  flags bitmask, labels text[],     -- 本地标注层，§9.2 说它永不被远端覆盖
  class_override smallint,          -- 人工改判；生效的 class = coalesce(class_override, messages.class)
  pinned bool                       -- 钉住：§9.6 卸载跳过
)

-- reparse 重新折叠时，被合并掉的旧 id 指向存活的 id，yuuka:// 锚点和外键引用因此不失效
message_aliases(old_id ulid pk, new_id → messages)

threads(id ulid pk, subject_norm, participants text[], first_at, last_at, n, class)

prefixes(
  zone text, prefix text, primary key(zone, prefix), first_seen, last_seen, label, note,
  expected_domains text[],          -- 我把它给了谁
  observed_domains text[],          -- 实际谁在发
  state smallint,                   -- claimed(我认领/给出去过) | discovered(只是有人往这投过)，§3.3
  leak_suspect bool, active bool
)

-- 投递：一封内容可以有多条（不同账户/不同前缀各收到一份）
-- 投递是 ingest 记下的事实，不是从 MIME 派生的：信封、远端定位、卸载状态都不在原件里
deliveries(
  id ulid pk,
  message_id → messages,             -- 可空：notify 策略只留幂等桩，不建 message（§10.1）
  account_id → accounts,
  raw_id → raw_objects,              -- 这一次投递自己的原件：Received / ARC / DKIM 每一路都不同
  auth jsonb,                        -- 只来自可信接收方（§10.1.1）；可由 reparse 重算
  zone text, prefix text,            -- catch-all 的核心维度，落在这一层
  envelope_from text, envelope_to text, delivered_to text[],
  mailbox text, uidvalidity bigint, source_uid text,   -- 远端定位 + 幂等（§10.1）
  remote_labels text[], remote_flags bitmask, received_at timestamptz,
  remote_state smallint,             -- present | offloaded（远端已按 §9.6 删除，本地是唯一副本）
  offloaded_at timestamptz,
  unique(account_id, mailbox, uidvalidity, source_uid)
)

attachments(id ulid pk, message_id, filename, mime, size, sha256, content_id, disposition, blob_key)
extractions(message_id, kind(otp|link|expiry|amount), value, confidence)  -- 通知区用
rules(id, priority, match jsonb, actions jsonb, enabled, proposal_id)     -- 分流/标签/推送；actions 封顶在标注层（§8.5）
drafts(id ulid pk, principal_id → principals, identity_id → identities, thread_id, in_reply_to,
       to_addrs, subject, body_md, attachments jsonb, proposal_id,
       cites jsonb,                  -- [{uri, quote}] 引用了哪些入站内容（§8.1 锚点），§8.9 外泄检查的输入
       hold_reason text,             -- 提交后为什么停在草稿：档位 / 限额 / 审核升级
       updated_at)
  -- 提交时按 identity_grants.send_policy 决定去向：留在这里等人，或生成 outbox 记录（§8.9）
outbox(id ulid pk, state, attempts, next_retry_at,
       -- state: held → queued → sending → sent | partial | unknown | failed(n) → dead
       --   held 只出现在 review 档；unknown 是「提交后没拿到结论」，不自动重试（§3 原则 6）
       identity_id → identities,     -- 路由由 identity 决定（§9.3），调用方选不了 transport
       submitted_by → principals,    -- 谁起的这封信 / 哪个服务从 submission 口投进来（§13.6）
       approved_by → principals,     -- 哪个人按了发送；按策略放行时是 system
       review jsonb, hold_until,     -- §8.9 每项检查的结论；撤回窗口截止时间
       draft_id → drafts, from_addr, raw_key,   -- raw_key 是我们提交的字节，不含平台加的头（§10.4.1）
       verp_tag text,                -- Return-Path 本地部分嵌的 id，DSN 回流靠它对账；出口不允许时为空（§10.4.2）
       provider_id text,             -- 出口给的投递 id，查状态用
       sent_message_id text,         -- 实际发出的 Message-ID；出口改写时回填，线程匹配读这里
       error)

-- 逐收件人的结果：部分收件人被拒、退信、投诉都落在这一层
outbox_recipients(outbox_id → outbox, addr, kind(to|cc|bcc),
                  state,              -- pending | accepted | rejected | deferred | bounced | complained
                  smtp_code, detail, updated_at,
                  primary key(outbox_id, addr))
notifications(id, message_id, channel, state, sent_at)
changes(seq bigserial pk, kind(message|delivery|thread|flag|prefix|proposal), target_id, at)
  -- 全局变更日志：§8.6 的 since_state、JMAP queryChanges、前端 SSE 共用。M1 建表就开始写
```

全文检索：Postgres 原生 FTS 对中文很弱。先用 `pg_trgm` 够个人用量，
中文体验不行再上 PGroonga 或外置 bleve 索引（这是可替换的一层，不影响模型）。

### 4.1 折叠键：Message-ID 单独不够

catch-all 意味着**任何人都能往任意前缀投递**，所以 `message_id` 不能单独做唯一键：
攻击者投一封 Message-ID 与已有邮件相同的信，就会被折叠进旧 message，新内容在索引层消失
（raw 还在，但 UI 看不到）；另外大量垃圾信没有 Message-ID 或反复重用同一个。因此：

- 折叠键是 `(message_id, body_hash)`。`body_hash` 对去掉传输头
  （`Received` / `Received-SPF` / `ARC-*` / `Authentication-Results` / `DKIM-Signature` /
  `Return-Path` / `Delivered-To` / `X-*`）后的原件字节取哈希。
  它只保证一个方向：**内容不同，哈希必然不同**，所以不会误合并。
  反方向只是「通常相同」：转发和邮件列表会改 Subject 或追加页脚，中继可能把 8bit 重编码成
  quoted-printable，这些都会让同一封信算出不同的哈希。**哈希不同就不合并**——
  代价是同一封信显示两次（它们仍按 Message-ID 进同一条线程），比误合并吞掉内容便宜得多。
  不改成对解码后内容取哈希：那会让折叠键依赖解析器版本，解析器一升级折叠结果就变
- 缺失 Message-ID 时合成 `<sha256@zone>`，既不丢信也不误折叠
- raw 的 sha256（`raw_objects.sha256 unique`）只负责**同一路径的重投去重**（Worker 重试、
  IMAP 重扫）。跨路径折叠不能靠它——每一路的 Received 头不同，sha256 必然不同
- §1 沿用的 `subject + sender` 兜底线程匹配**只对 `class=human` 启用**：
  机器邮件里同一发件人的所有「Your verification code」会被它归成一条巨型线程

### 4.2 重建边界：reparse 重建什么、不碰什么

§3 原则 1 说的「可重建」只覆盖从邮件内容派生的东西。边界按表划清，不在同一张表里混放：

| | 表 | reparse |
|---|---|---|
| **派生** | `messages`（除 id 外的列）、`threads`、`attachments`、`extractions`、`deliveries.auth` | 从 `deliveries.raw_id → raw_objects` 重算 |
| **投递事实** | `deliveries` 的其余列（信封、远端定位、`remote_state`、`offloaded_at`） | 不碰。它们来自 ingest 当时的 `InboundRef`，不在原件里 |
| **决定** | `message_state`、`message_aliases`、`prefixes`（label / note / expected_domains / state）、`rules`、`proposals`、`actions`、`drafts`、`outbox`、`notifications` | 不碰 |
| **配置与授权** | `principals` / `credentials` / `identity_grants` / `account_access`（§13）、`accounts` / `identities` / `transports` / `zones` | 不碰 |

后三类**无法从原件重建**，备份要按正经数据库来（§7）。CF 来源的投递事实能从 R2 对象的
metadata 恢复（§2.1），但外部账户的远端定位和卸载状态只在库里，同样要靠备份。

`reparse` 的几条规则：

- **message id 跨 reparse 稳定**。重算按现有 id 原地更新；折叠规则变化导致两条合并时，
  保留较早的 id，被合并的写进 `message_aliases`；拆分时原 id 留给 canonical 投递那一侧。
  否则 `message_state`、`proposals.cites`、`actions` 和人手里的 `yuuka://` 链接全部失效
- 段落锚点 `#part/2/para/7` 依赖解析器的分段方式，可能随 `parser_version` 漂移。
  所以 `cites` 同时存锚点和一小段原文摘录，锚点对不上时按摘录重新定位
- 生效的分类是 `coalesce(message_state.class_override, messages.class)`：
  人的改判永远压过重算结果，规则重跑也不会把它改回去

### 4.3 垃圾判定要有来源

一个 catch-all 域每天会收到成百上千随机前缀的字典攻击。没有垃圾判定，
§3.5「未知前缀照收并自动登记」会让 `prefixes` 被淹没、TG 每封都响、泄漏检测的基线全是噪声。
Cloudflare Email Routing 基本不做内容过滤，`spam_score` 字段必须有人填：

- M1 最低实现：`dmarc=fail`、或 `prefixes.state=discovered`（§3.3）→ 直接降级，
  不推送、不进聚合视图、不参与泄漏基线，只静默入库
- 需要更强判定时旁路 rspamd（标准协议，ingest 多一步调用），不影响模型

### 4.4 只有头部的信

`sync_policy=metadata` 的账户只落头部（§9.2、§10.1）。这带来三个存储层的问题：

**建消息**：`raw_objects.completeness = header_only`，照常建 `messages` 行，
但 `body_hash` 为空、`body_text` 为空、`has_attach` 按 `Content-Type` 推断。
`body_hash` 为空的行**不参与跨账户折叠**：只凭头部无法证明两份是同一内容，
而 §4.1 的方向是宁可重复、不可误合并。它们仍按 Message-ID / References 进同一条线程，
UI 上就是线程里并列的两条，其中一条标「正文未同步」。

**按需取正文**：`FetchBody` 的结果是流，只存在于这一次响应里：

- 不写 RawStore、不写 `messages.body_text`、不进全文索引
- HTTP 响应带 `Cache-Control: no-store`；前端不写入 IndexedDB（§12.4）
- `actions` 记下「谁、在什么时候、取了哪条投递的正文」，`result` 只记大小和 sha256，**不记内容**
- agent 取正文要有该账户的 `account_access`，并按 §9.4 计为一次外部请求

**补全**：账户从 `metadata` 改成 `full` 时，后台任务按投递重新拉取完整原件，
写一份新的 `raw_objects`（`completeness=full`），把 `deliveries.raw_id` 指过去，
再对这条投递跑 reparse——此时它有了 `body_hash`，可以正常折叠。
旧的 header_only 原件保留（原件不可变），只是不再被引用。

`notify` 策略的账户不走这里：它不建 `messages`，只有 `deliveries` 的幂等桩和 `notifications`。

---

---

### 9.1 内容 / 投递两层模型（§4 已改）

同一封信同时落到 `x@example.com` 和 Gmail（CC、多地址、转发链）是常态。所以：

- `messages` 存**内容**，按 `(message_id, body_hash)` 跨账户折叠成一条（§4.1）
- `deliveries` 存**每次投递**：哪个账户、哪个域、哪个前缀、IMAP UID、
  那边的 folder/label、那边的已读状态，以及**这一路自己的原件和认证结果**
  （`raw_id` / `auth`——两条投递的 Received、ARC、DKIM 验签各不相同，不能只留一份）

好处：
- 收件箱里一封信就是一封信，不是三条重复
- 但"它是通过哪个前缀/账户来的"完整保留 → 前缀泄漏检测（§3.3）照样成立
- 幂等键天然是 `(account_id, mailbox, uidvalidity, source_uid)`（§10.1），重复拉取不产生重复信

跨账户线程归并：按 `Message-ID / In-Reply-To / References` 构图，
**线程不属于任何账户**。一条线程里前半在 Gmail、后半在自有域的情况能正确拼起来。
