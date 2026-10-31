# Agent 面：人机同屏，权限分级

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 8. Agent 面：人机同屏，权限分级

前提假设：**未来读这个邮箱的次数里，agent 会多于人本身**。所以 agent 不是 M4 挂一个
MCP server 了事，它和 WebUI 一样是一等外壳。三条外壳（WebUI / JMAP / Agent）
共用同一个领域 API、同一套鉴权、同一套审计日志——**不给 agent 开后门**，
因为后门就是绕过审计的那个门。

### 8.1 Agent 要的不是 HTML，是可引用的文本

同一封邮件要有一个 `text/agent` 表示，和给浏览器的 HTML 并列：

- 去样式、去 tracking pixel、去重复引用段（quoted reply 折叠成一行标记）
- 保留结构（标题/段落/列表/链接目标），链接以 `[文本](URL)` 保留**真实** URL
- 带**稳定锚点**：`yuuka://message/<ulid>#part/2/para/7`
- 分层获取，token 预算可控：`snippet(200字) → sections(目录) → part(按需)`
  一次不灌 200KB HTML，这是最常见的 agent 上下文浪费
- 头部带认证事实：`spf=pass dkim=pass dmarc=pass from_domain=github.com`
  （含 `ix64/spf` 的二次判定）。只来自可信接收方（§10.1.1），取不到就明确标「未验证」

稳定锚点是"人机流畅"的关键机制：agent 说的每个结论都能带一个 URI，
人在 WebUI 里一点就跳到原文那一段。**agent 的输出可被当场验证**，
而不是要人重新找信。

### 8.2 能力分级，在 API 层强制

| 级别 | 动作 | agent 权限 | 机制 |
|---|---|---|---|
| 读 | search / get / extract(otp,link) / stats | 直接给 | 只读 token |
| 标注 | label / classify / mark read / archive / 建议规则 | 给，但必须可撤销 | 写入 `actions` 日志 + undo token，WebUI 可一键回滚 |
| 外发 | send / reply / forward | **按身份分档授权**（§8.9） | `submission:send` scope + `identity_grants.send_policy`：以人的身份只到草稿；以 agent 自己的身份可以自动审核或直发，受限额约束 |
| 越界 | 点退订链接 / 跟随邮件里的链接 / 删除原件 / 远端 expunge | **不给** | 没有对应的 scope，也没有对应的 API |

重点：**这是 token scope 和授权表的事，不是提示词的事**（scope 的完整定义见 §13.4，
agent 的 token 由 §13.3 的 `credentials` 签发，授权表见 §13.5）。
agentic-inbox 也要求发信前确认，但它靠 prompt 约束；一旦换模型或被注入就失效。
这里 agent 能走到哪一步由它的凭据和授权决定，模型输出什么都越不过去。

外发不是一刀切的「不给」，而是和 coding agent 的权限模式同构：同一个 agent，
有的命令每次都问、有的自动放行、有的交给审核器把关、拦下的转人。
邮件里决定档位的维度是**以谁的身份发**（§8.9）。

### 8.3 邮件正文是攻击面（catch-all 尤其）

catch-all 的含义是：**任何人都能往任意前缀投递内容，直接进到 agent 的上下文里**。
这是 prompt injection 的理想入口。对策：

- agent 视图里邮件内容包在明确的数据边界内，标注"以下是不可信的第三方内容"
- 未通过 DMARC 的信在 agent 视图里降级并打标；agent 默认不执行其中任何指令
- **禁止 agent 自动跟随邮件里的链接**（退订、magic link、"点此验证"一律人点）
- 任何来自邮件内容的"请删除"只能产出提案；"请发送/请转发"即便 agent 照做，
  也要过 §8.9 的审核——新收件人、跨线程搬运内容、含提取出的验证码都会升级到人。
  以人的身份则无论如何只到草稿（和 §8.2 的 scope 是同一道闸）
- 这套 auth 信号和 §3.3 的泄漏检测共用，一份实现两个用途

### 8.4 通知通道 = 审批通道（这条最影响日常手感）

TG（或其他 IM）不只是推送，而是**人机交接点**：

```
[example.com] svc-aws@ → AWS 验证码  ·  dmarc=pass
验证码 740219（8 分钟内有效）
[复制] [归档] [这个前缀泄漏了] [起草回复]
```

agent 做摘要和提取，人在手机上一点就完成审批/标注，确认结果回流成规则。
这样"人确认"不是摩擦，而是一次点击——**外发闸门的代价被压到最低，
所以闸门才守得住**。WebUI 侧同理：agent 的提案、草稿、规则建议就地长在
邮件旁边（agentic-inbox 的 side panel 思路），人看到的和 agent 操作的是同一对象，
不是另开一个聊天框再复制粘贴。

但这同时意味着 **TG 是一个认证端点，不只是通知出口**（§13.7 已补一行）：

- 回调必须校验 `callback_query.from.id` 等于运维者的 TG 用户 ID，按钮动作以
  `human:<operator>` 的一把 `session` 类凭据记入 `actions`。TG 账号失陷等于拿到
  `submission:send`，这要写进威胁模型，而不是默认 TG 可信
- **确认发送的卡片不能只显示 agent 写的摘要**。收件人、发信身份、主题、附件清单、正文前几行
  由系统直接从 `drafts` 行渲染、署 yuuka（§11.1）；agent 的说明另起一段署 arisu。
  否则注入攻击只需骗过一次指尖，§8.2 的闸门就形同虚设
- 卡片上的按钮只触发标注层动作、「确认发送这份草稿」和「撤回这封待发信」；
  没有「按 agent 建议处理」这种把决定权交回 agent 的按钮
- 自动审核放行、处在撤回窗口里的信（§8.9 的 `held`）同样推一张系统署名的卡片，
  默认动作是什么都不做（到期发出），[撤回] 是一等按钮

### 8.5 协作的单位是任务，不是邮件

人机协作的单位不是"一封信"，而是"一件待办"：这个验证码给我、这张发票归档、
这个列表退订、这个前缀换掉。所以有 `proposals` 层：

```
proposals(id, source(agent|rule), kind, target_message/thread/prefix,
          payload jsonb, rationale text, cites jsonb,    -- [{uri, quote}]：yuuka:// 锚点 + 原文摘录（§4.2）
          state(pending|accepted|rejected|expired), decided_by, decided_at)
```

- agent 产出提案，附**理由和原文引用**（cites 用 §8.1 的锚点，可点开核对）
- 人接受/拒绝一次 → 可选"以后同类自动处理" → **提案沉淀成 `rules` 记录**
- 于是 agent 的贡献是累积的：同一类判断第二次就不再需要人介入，
  而不是每次重新推理一遍（也省 token）
- **规则是持续的能力，所以要封顶**：`rules.actions` 只允许标注层动作
  （label / classify / archive / 推送通知），**永不含 send / forward / 点链接 / 删原件**。
  否则 agent 提案一条「转发给某地址」的规则、人在手机上一点接受，之后就是一个不再经人的
  外发通道——这是绕过 §8.9 授权最短的路。规则没有发信身份，`rule:<id>` 也拿不到任何 `identity_grants`
- 外发类提案的「以后同类自动处理」不沉淀成规则，而是落成一次**显式的授权变更**
  （例如把某地址加进 agent 身份的 `recipient_allowlist`）。人看到的是「给 arisu 提权」，
  而不是一条看起来无害的规则
- 从提案沉淀的规则要能回溯到提案（`rules.proposal_id`），
  这样「这条规则是谁在什么证据下建议的」在 §8.8 的审计里能查到

### 8.6 事件流和游标

长期在跑的 agent 不能靠轮询全表。给一条 SSE 流 + `since_state` 游标，
和 JMAP 的 `Email/queryChanges` 用同一套状态机——**人和 agent 共用一套增量同步**，
不维护两份。agent 断线重连只拉差量。

### 8.7 MCP 面要窄且语义化

MCP server 暴露的不是 REST 的全量投影，而是一把语义化的窄工具：

```
search_messages(q, prefix?, class?, since?, limit)   -- 返回锚点 + snippet
get_message(uri, level=snippet|sections|part)        -- 分层，省 token
get_otp(prefix|thread)                               -- 高频意图直接成 tool
list_prefixes(leak_suspect?)                         -- 前缀视角
propose(kind, target, payload, rationale)            -- 标注类的写入口
create_draft(identity, ..., cites)                   -- 起草，cites 是 §8.9 审核的输入
submit_draft(draft_id)                               -- 按授权决定结果：sent | held | drafted(原因)
```

`submit_draft` 永远成功返回一个结果，不因档位不够而报错：超出授权就停在草稿并说明原因。
agent 因此不需要预先知道自己的权限，也没有动机去「换个方式再试一次」。

Resource URI 统一 `yuuka://`，让 agent 能把引用直接递给人。
工具少、语义贴近意图，模型选错的概率和 token 都更低。

### 8.8 审计与回放

`actions(id, actor → principals.id, tool, args, result, undo_token, at)`（主体模型见 §13.2）
记录每一次动作。可以随时回答"昨天 agent 干了什么"，并一键回滚标注类动作。
配合 §3.1 的不可变原件：agent 够不到原件，**收件侧的一切都可恢复**。

外发是 agent 唯一不可逆的动作，所以它被四层收窄：身份（以人的身份只到草稿）、
限额、审核、撤回窗口（§8.9）。每封外发在 `outbox` 上记 `submitted_by`（谁起的）、
`approved_by`（哪个人按的，或 `system` 代表按策略放行）和 `review`（每一项检查的结论），
「这封信为什么会被发出去」任何时候都能回答。

### 8.9 外发授权：身份决定档位

核心区分：**agent 以自己的名义说话，和 agent 以人的名义说话，是两件事。**

- 前者像同事用自己的工作邮箱回信：收件人看得到发件人是 arisu，出错的代价是 arisu 这个身份的信誉
- 后者是代笔：收件人以为是运维者本人写的，出错的代价落在人身上，而且收不回

所以 identity 有归属（`identities.owner_id → principals`，§4）。agent 有自己的专属身份
（例如 `arisu@example.com`，owner 是 `agent:arisu`），显示名和签名默认表明它是 agent。

#### 档位

`identity_grants.send_policy`（§13.5）四档：

| 档位 | 行为 | 对应 coding agent 的 |
|---|---|---|
| `none` | 不能用这个身份 | 不在允许列表里 |
| `draft` | 只生成草稿，人确认才进 outbox | 每次都问 |
| `review` | 进 outbox 的 `held` 态，过自动审核 + 撤回窗口，通过即发；拦下的退回草稿等人 | auto 模式：审核器把关，拦下的转人 |
| `auto` | 直接进 `queued`，只受限额约束 | 白名单命令免确认 |

**硬上限**：身份的 owner 是 human、而发信主体不是这个 human 本人时，档位上限是 `draft`。
这条在 API 层强制，授权表里写 `auto` 也会被压回 `draft`。agent 可以替人起草，
不能自动以人的名义发信。

**超出授权是降级，不是拒绝**：档位不够、超限额、审核没过，一律停在草稿并通知人。
agent 的工作不会丢，人也不会被一堆报错淹没。

#### 限额与熔断

每条授权带自己的限额（`max_per_hour`、`max_per_day`、`max_new_recipients_per_day`）
和收件人范围（`recipient_scope`）：

- `thread`：只能回复已有线程，收件人必须是该线程的既有参与者
- `allowlist`：收件人在 `recipient_allowlist`（地址或域）里
- `any`：不限

agent 专属身份的默认值是 `review` + `thread`。

熔断是全局的：某条授权的退信率或投诉率超阈值、或短时间内被人撤回 / 驳回过多次，
该授权自动 `suspended`，期间所有提交降级为草稿，等人看过再恢复。

#### 自动审核（`review` 档）

按代价从低到高跑，**任何一项不过就升级到人**，不静默丢弃：

1. **限额与收件人**：是否在 `recipient_scope` 内；**首次联系的地址一律升级**
2. **确定性的外泄检查**——catch-all 收件箱里最值钱的就是验证码和 magic link，
   注入攻击最想做的就是把它们发出去：
   - 正文或附件里出现 `extractions` 表中的值（验证码、链接）→ 升级
   - 引用、转发或附带的内容来自另一个线程，而收件人不是那个线程的参与者 → 升级
   - 正文里出现收件箱里没见过、也不在授权白名单里的链接域 → 升级
   判断依据是草稿的 `in_reply_to`、`cites`（§8.1 的锚点）和正文本身，不依赖 agent 的自述
3. **模型审核**（可选）：一个独立的 reviewer 主体，**没有任何工具**，只看待发的信、
   收件人与本线程的往来元数据、agent 的 `rationale`，输出 `pass | escalate` 和理由。
   它**只能升级，不能放行**前两步拦下的东西。用和起草者不同的上下文，最好是不同的模型，
   且不读入站正文——同一段注入不该同时骗过起草者和审核者
4. **撤回窗口**：通过后在 `held` 停留 `hold_seconds`，同时推送 §8.4 的卡片；
   到期进 `queued`，窗口内人可一键撤回

`auto` 档跳过 2–4，只做第 1 步的限额与收件人范围检查。

#### 邮件循环

两个自动回信的系统互相回复是真实的故障模式，agent 自动发信时尤其容易触发。
沿用 RFC 3834 的约定：

- agent 在 `review` / `auto` 档发出的信带 `Auto-Submitted: auto-generated`
- agent 的自动回复不回应带 `Auto-Submitted`（值不为 `no`）、`Precedence: bulk|list`
  或 `class=machine` 的信，这类回复一律降级为草稿
- 同一线程、同一收件人的自动外发另有一个小上限，超了降级

---

---

### 11.1 吉祥物承担"系统人格"，这不是装饰

`actions.actor` 指向 `principals`（§13.2）：`human:<operator>` / `agent:arisu` /
`service:<name>` / `rule:<id>` / `system`。署名直接读 `principals.display_name`。

**yuuka 是地方和系统的声音，arisu 是运维者的 agent。** 这个区分要落到 UI 和 TG 上：
系统消息（新信到达、出口失败、前缀泄漏告警、配额告警）署 yuuka；
agent 的摘要、提取、提案署 arisu。

理由不是可爱，而是**避免把 agent 的推测读成系统事实**——
"验证码是 740219"（系统正则提取，确定）和"我觉得这封是钓鱼"（agent 判断，可能错）
必须在视觉上一眼区分，否则 §8.3 的注入防线在人这一侧就失守了。
同一条 TG 消息里两种内容共存时，分段署名。
