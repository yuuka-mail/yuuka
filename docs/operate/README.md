# 运维

写给部署和管理 yuuka 的人：怎么配、怎么管、出了问题怎么查。这里不讲设计理由，需要时链到[设计文档](../design/README.md)。

| 文档 | 内容 |
|---|---|
| [cloudflare.md](cloudflare.md) | R2 的 bucket 与令牌、Email Worker、发信出口 |
| [config.md](config.md) | 全部环境变量 |
| [cli.md](cli.md) | 管理命令：登录、前缀、发信地址与授权、凭据、发件队列、分诊评分、维护 |
| [submission.md](submission.md) | SMTP 提交口的暴露范围与 TLS |
| [troubleshooting.md](troubleshooting.md) | 按现象排障 |

用 Docker Compose 部署的具体做法（服务、首次启动、备份恢复、升级、端口）在 [deploy/README.md](../../deploy/README.md)。

## 上线顺序

1. 在 Cloudflare 建两个 R2 bucket 和两把令牌（[R2](cloudflare.md#r2两个-bucket两把令牌)）
2. 部署 yuuka 本体，确认 `/healthz` 返回 200（[deploy/README.md](../../deploy/README.md)）
3. 部署 Email Worker，把域的 catch-all 指向它，发一封测试信（[Email Worker](cloudflare.md#email-worker收信)）
4. `yuuka admin enroll` 注册第一个 passkey，离线保存恢复码（[第一次登录](cli.md#第一次登录)）
5. 认领你在用的前缀（[前缀](cli.md#前缀)）
6. 要发信：onboard Email Sending、配出口、建发信地址（[Email Sending](cloudflare.md#email-sending发信)、
   [发信地址与授权](cli.md#发信地址与授权)）
7. 让 agent 和其他服务接入（[接入文档](../integrate/README.md)）
