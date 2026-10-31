# 设计文档索引

写给开发者：系统为什么是现在这个样子，哪些是不变量，放弃了什么。
怎么部署、怎么接入不在这里，见 [docs/README.md](../README.md)。

整份设计原本是一个文件，拆分后**节号保持全局唯一**（§1–§15），
所以文档之间、以及代码注释里的 `§8.3`、`§10.2.1` 这类引用都指这里，按下表定位即可。
节号是稳定标识：调整章节时保留旧号，新内容取新号，不重新编号。

| 节 | 文档 | 内容 |
|---|---|---|
| §1 | [prior-art.md](prior-art.md) | 现有 Cloudflare 邮件项目为何不适用本用例 |
| §2 §3 §5 | [architecture.md](architecture.md) | 总体架构、八条设计原则、Go 侧选型、API 契约 |
| §4 §9.1 | [data-model.md](data-model.md) | Postgres 数据模型；内容/投递两层拆分 |
| §6 §7 | [roadmap.md](roadmap.md) | 分期路线 M0–M5、已知风险与取舍 |
| §8 §11.1 | [agents.md](agents.md) | Agent 面：锚点、能力分级、提案、审计、外发授权、人格分层 |
| §9 | [accounts.md](accounts.md) | 多账户整合：统一视图、分级归属、身份绑传输、归档卸载 |
| §10 | [drivers.md](drivers.md) | 收发 driver 契约、能力矩阵、抽象漏出的四件事 |
| §11 | [naming.md](naming.md) | 命名、命令面、仓库与 module path |
| §12 | [frontend.md](frontend.md) | 前端选型、邮件渲染两条路径、PWA |
| §13 | [principals.md](principals.md) | 多主体、凭据、scope、SMTP submission |
| §14 | [brand.md](brand.md) | 吉祥物、logo、品牌色与美术资源授权 |
| §15 | [triage.md](triage.md) | 分诊：决策模型判断垃圾、欺诈与分区，评分是观测事实 |

## 阅读顺序

初次了解：[architecture.md](architecture.md) → [data-model.md](data-model.md) →
[agents.md](agents.md)。想知道能不能换掉 Cloudflare：[drivers.md](drivers.md)。
