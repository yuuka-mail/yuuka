# 接入 MCP

yuuka 给 agent 一组窄的、按意图设计的工具：检索、按段落阅读、起草、提交。
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

向运维者要一把 token（`yuuka token issue`），scope 只要用得到的：

| scope | 能做什么 |
|---|---|
| `message:read` | `search_messages`、`get_message` |
| `delivery:read` | `get_message` 里多出投递事实：收件人、SPF / DKIM / DMARC 结果 |
| `prefix:read` | `list_prefixes` |
| `draft:create` | `list_identities`、`create_draft`、`update_draft`、`submit_draft` |
| `submission:send` | 让提交**有机会**真正发出；没有它，每次提交都停在草稿 |

只读的 agent 要前三个。要它起草再加 `draft:create`，要它的提交可能被发出再加 `submission:send`。
多给的 scope 不会让工具多做什么，只扩大泄漏面。

有了 `submission:send` 也不等于能发：还要发信地址上有给这个 agent 的授权，而以人的地址发信最多到草稿。
agent 不用预先知道这些，`list_identities` 会告诉它。

## 工具

| 工具 | 作用 |
|---|---|
| `search_messages` | 按主题、发件人、正文里的词检索，返回 `yuuka://` URI 和摘要，最新的在前。默认不含降级的信（未认领前缀、DMARC 失败），`include_downgraded` 打开 |
| `get_message` | 读一封信。`level=snippet` 给事实和摘要，`sections` 给目录，`part` 从锚点那一段开始给正文 |
| `list_prefixes` | 列出收信的前缀、信数和运维者的标注；已认领的前缀带期望的发件域、DMARC 证实过的发件域和泄漏 / 仿冒数，`leak_suspect` 只列出过事的 |
| `list_identities` | 你能用哪些地址写信，每个地址提交后是「会发出」还是「停在草稿」，以及原因 |
| `create_draft` | 起草，不发出。回复时不给 `from` 就用原信进来的那个地址。把草稿依据的段落写进 `cites`，审核的人会核对 |
| `update_draft` | 整体替换你自己的、还没提交的草稿 |
| `submit_draft` | 提交草稿。结果是 `queued`（会发出）或停在草稿并给出原因 |

### 锚点

`get_message` 返回的正文按段落带稳定锚点，形如 `yuuka://message/<id>#part/<section>/para/<n>`。
把它原样交给人，人在 WebUI 里能跳到同一段；写进 `cites` 时也用它。重新解析之后锚点不变。

### 停在草稿是正常结果

`submit_draft` 不会因为档位不够而报错。结果里的原因可能是：

- `owner`：这是人的地址，agent 永远不能以人的身份直接发出
- `policy`：凭据没有 `submission:send`，或授权本身就是草稿档
- `review`：这个地址要求审核
- `limit`：超出了每小时、每天或新收件人的限额
- `recipients`：收件人超出了允许的范围
- `suspended`：运维者暂停了这条授权
- `loop`：防邮件循环。回复的是机器发的信（自动回复、群发、通知），或者同一线程、同一收件人 24 小时内已经自动发过 3 封
- `withdrawn`：审核期间被撤回

信已经留给人处理了。**不要换一个工具、一个地址或另一条路再发一次**。

## 审计

每次调用都记一条审计：工具名、参数、结果的计数和 URI。读也记，因为「agent 看了什么」同样要能回答。
起草和提交只记地址、收件人个数和引用个数，不记主题和正文。
