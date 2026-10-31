# 接入 SMTP

让只会发邮件的服务（监控、告警、定时任务）经 yuuka 发信，不用改它的代码：配一个 SMTP 服务器、用户名和密码就行。
所有服务的信因此进同一个已发送归档，共用一套限速和重试，凭据可以单独撤销。
提交口要由运维者先打开（[SMTP 提交口](../operate/submission.md)）。

## 准备：地址、授权、密码

每个服务一个自己的发信地址、一条授权和一把密码。由运维者在服务器上做：

```bash
yuuka identity add alerts@example.com --owner service:monitoring --transport cloudflare
yuuka grant set --identity alerts@example.com --principal service:monitoring \
  --policy auto --recipients allowlist --allow ops@example.com
yuuka token issue --kind smtp --principal service:monitoring --label alertmanager
```

最后一条打印两样东西：**用户名是凭据 id**，密码是 `ykp_` 开头的那一串，只显示这一次。

地址的 `--owner` 要写这个服务本身。以运维者本人的地址发信，非本人提交的一律停在草稿，服务的信就发不出去。
新接入的服务可以先给 `--policy draft`，观察一阵它发了什么再调成 `auto`：它照样收到 `250`，信停在草稿里等人看。
档位、收件人范围、限额的写法见[管理命令](../operate/cli.md#发信地址与授权)。

## 客户端配置

| 项 | 值 |
|---|---|
| 主机 | 提交口证书上的名字，如 `mail.example.com` |
| 端口 | `465` |
| 加密 | 「SSL/TLS」或「implicit TLS」，**不是 STARTTLS** |
| 认证 | AUTH PLAIN；用户名是凭据 id，密码是 `ykp_…` |

## 信头会被怎样处理

- **按信头 `From` 判断用哪个地址**，不按信封的 `MAIL FROM`。`From` 必须恰好一个地址，且这把密码的主人对它有授权；
  有 `Sender` 头时它也要有授权；`MAIL FROM` 必须和 `From` 相同
- `From` 的显示名换成发信地址配置的显示名，客户端写的不算
- `Bcc` 头被删掉，收件人只看信封
- 缺 `Date` 就补上；`Message-ID` 按出口的能力补或删
- 裸 CR、NUL、超过 998 字节的行直接拒收，不替你修
- 单封信最大 25 MiB，一个事务最多 50 个收件人，第 51 个回 `452`，客户端会把剩下的放进下一个事务
- 不支持 `SMTPUTF8`：信封地址必须是 ASCII

## 回复码

| 回复 | 含义 | 客户端该做什么 |
|---|---|---|
| `250 2.0.0 Queued as <id>` | 进了发件队列，会发出 | 无 |
| `250 2.0.0 Held for review as <id>: <原因>` | 停下等审核 | 无，等人处理 |
| `250 2.0.0 Kept as draft <id>: <原因>` | 停在草稿 | 无，等人处理 |
| `550 5.7.1 <原因>` | 拒收：没有地址对得上 `From`、授权是 `none`，或违反上面的规则 | 修配置；重试没用 |
| `451 4.3.0` | yuuka 内部暂时出错 | 客户端照常稍后重试 |
| `552 5.3.4` / `452 4.5.3` | 超过大小 / 收件人上限 | 拆小 |
| `535 5.7.8` | 认证失败 | 检查用户名（凭据 id）和密码 |
| `454 4.7.0` | 认证被限速或认证本身出错 | 稍后重试 |
| `530 5.7.0` | 没认证就发 `MAIL` | 打开 AUTH |
| `421` | 连接数超限，或这把密码在会话中途被撤销 | 重连；被撤销的会在 AUTH 得到 `535` |

**停在草稿或待审核也回 `250`**。这是授权在起作用：回错误的话客户端会重试，草稿越堆越多。
回复里的 id 让运维者找到这封信。
