# 文档

按你要做的事找入口。全部是中文。

| 你是 | 要做的事 | 从这里开始 |
|---|---|---|
| 服务器管理员 | 部署、配置、日常管理、出了问题怎么查 | [operate/](operate/README.md)；用 Docker Compose 部署看 [deploy/README.md](../deploy/README.md) |
| agent 作者、要发信的服务 | 接 MCP、SMTP、REST | [integrate/](integrate/README.md) |
| 开发者 | 改代码、理解设计为什么是这样 | [CONTRIBUTING.md](../CONTRIBUTING.md)、[design/](design/README.md) |

几类文档各管一件事：

- **运维、接入文档**回答「怎么做」。写的是步骤和参考，不讲设计理由，需要时链到设计文档
- **设计文档**（[design/](design/README.md)）回答「为什么」：不变量、取舍、放弃了什么。
  已经实现的部分以代码为准，设计文档只链过去。节号 §1–§17 全局唯一，代码注释里的 `§x.y` 指的就是它
- [deploy/README.md](../deploy/README.md) 只讲 Docker Compose 这一种部署方式的具体做法，放在 compose 文件旁边

## 约定

- `example.com` 代表任意一个自有 catch-all 域名
- `agent:arisu` 是示例 agent 标识；`human:<operator>` 是运维者本人；
  `service:<name>` 是机器主体，如 `service:monitoring`
- 公开文档不含真实域名、账户清单和部署细节
