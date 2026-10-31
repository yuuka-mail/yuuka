# 用 Docker Compose 部署

一台有固定公网地址的机器上跑整套 yuuka：Caddy 终结 TLS 并反代，yuuka 本体，Postgres，
以及定时 `pg_dump` 的备份服务。这只是部署方式之一；yuuka 本身不假设部署形态（§2），
每个端点自己认证（§13.7），Caddy 不是安全边界。

| 服务 | 作用 | 网络 | 卷 |
|---|---|---|---|
| `caddy` | 唯一对外发布端口（80/443），自动申请和续期证书 | `edge` | `caddy-data`、`caddy-config` |
| `yuuka` | `yuuka serve`：webhook、reconcile、WebUI、REST、MCP | `edge` + `backend` | `yuuka-data`（本地 raw 副本） |
| `postgres` | 官方镜像，按 digest 固定 | `backend` | `postgres-data` |
| `backup` | 定时 `pg_dump`，见 [`backup.sh`](backup.sh) | `backend` | `backups` |

`backend` 是 `internal` 网络，没有出口也不可从外部进入；yuuka 的端口 31480 只在容器网络里监听
（镜像里 `YUUKA_LISTEN=0.0.0.0:31480`），不映射到宿主机。

## 首次启动

前提：Docker Engine 与 compose 插件；`YUUKA_WEB_ORIGIN` 的主机名（如 `mail.example.com`）已解析到本机。

```bash
# 把 deploy/ 目录放到服务器上，然后在其中：
cp compose.env.example .env        # 镜像、WebUI origin、Postgres 密码、备份策略
cp yuuka.env.example yuuka.env     # R2、webhook 密钥、Telegram……
chmod 600 .env yuuka.env
$EDITOR .env yuuka.env
docker compose up -d
docker compose logs -f yuuka       # 看到 listening 即可；迁移在启动时自动执行
```

发布版本之前没有现成镜像，在仓库根目录自己构建，并在 `.env` 里写 `YUUKA_IMAGE=yuuka:local`：

```bash
docker build --build-arg VERSION=$(git describe --tags --always) -t yuuka:local .
```

Email Worker 的 webhook 地址是 `<YUUKA_WEB_ORIGIN>/ingest/cloudflare`，密钥与 `YUUKA_WEBHOOK_SECRETS` 一致。
这个路径必须能从 Cloudflare 访问，不要在它前面加 IP 白名单或浏览器登录（见 [`Caddyfile`](Caddyfile)）。

## 管理命令

管理命令在主机上用 `docker compose run` 跑，复用 `yuuka` 服务的环境；能跑它们就是最高权限（§13.7）。

```bash
# 第一个 passkey：打印一次性注册链接和离线恢复码
docker compose run --rm yuuka admin enroll

# 认领前缀（§3.3）
docker compose run --rm yuuka prefix claim shop@example.com
docker compose run --rm yuuka prefix ls --state discovered

# 给 agent 签发 bearer token（只显示一次），用于 <origin>/mcp 与 /api/v1/
docker compose run --rm yuuka token issue --principal agent:arisu \
  --scope message:read,delivery:read,prefix:read --label laptop
docker compose run --rm yuuka token ls
```

`actions` 里记录的 `host_user` 在容器里恒为 `nonroot`，不区分是谁敲的命令。

## 备份

Postgres 里有无法从原件重建的数据（§4.2、§7），备份是必需的。`backup` 服务每
`BACKUP_INTERVAL` 秒（默认 86400，即每天 00:00 UTC）做一次 `pg_dump --format=custom`，
确认 `pg_restore --list` 能读回后才落成 `yuuka-<UTC 时间>.dump`，并只保留最新的 `BACKUP_KEEP` 个（默认 14）。
按个数而不是按天数清理：备份连续失败时旧的不会被逐个过期删光。超过两个周期没有新备份时，
`docker compose ps` 里 `backup` 显示 `unhealthy`。

备份卷和数据在同一块盘上，**不算异地备份**。把它定期复制出这台机器（restic、rsync 等），
卷在宿主机上的位置：`docker volume inspect yuuka_backups --format '{{.Mountpoint}}'`。
备份里含凭据哈希和 token 密文（§13.3），按敏感数据保管。
R2 里的原件和 `yuuka-data` 卷里的本地副本是邮件内容本身，另由 R2 保存，不在 `pg_dump` 里。

```bash
docker compose exec backup sh /usr/local/bin/yuuka-backup once   # 立即备份一次
docker compose run --rm --no-deps --entrypoint ls backup -l /backups
docker compose run --rm --no-deps -T --entrypoint cat backup /backups/yuuka-20250101T000000Z.dump > yuuka.dump
```

## 恢复

恢复到一个空库：原地回滚时先删库重建；灾难恢复时就是新机器上 `docker compose up -d postgres` 得到的新卷。

```bash
docker compose stop yuuka backup
docker compose exec postgres dropdb -U yuuka --force yuuka    # 新机器上跳过这一步
docker compose exec postgres createdb -U yuuka yuuka           # 新机器上跳过这一步
# 备份卷里的 dump：
docker compose run --rm --entrypoint pg_restore backup \
  --dbname=yuuka --no-owner --exit-on-error --single-transaction /backups/yuuka-20250101T000000Z.dump
# 或者从机器外拷回来的 dump，经 stdin：
docker compose run --rm -T --entrypoint pg_restore backup \
  --dbname=yuuka --no-owner --exit-on-error --single-transaction < yuuka.dump
docker compose up -d
```

`--single-transaction` 让恢复要么全部成功、要么什么都不改。恢复后，备份时刻之后到达的邮件仍在 R2 里，
`docker compose run --rm yuuka reconcile --full` 会把它们补回来。**定期演练一次恢复**，没恢复过的备份不算备份。

## 升级

镜像在打版本 tag 时发布到 `ghcr.io/yuuka-mail/yuuka:<版本>`。升级就是改 `.env` 里的
`YUUKA_IMAGE`（建议连 digest 一起写），然后：

```bash
docker compose exec backup sh /usr/local/bin/yuuka-backup once   # 先备份
docker compose pull yuuka && docker compose up -d yuuka
```

迁移在启动时执行且只追加，旧版本不认识新表结构；回退版本要连同升级前的备份一起恢复。
Caddy、Postgres 的镜像 digest 写在 `compose.yaml` 里，随仓库更新。Postgres 跨大版本不能直接换镜像，
要 `pg_dump` 后恢复到新版本的空卷（或 `pg_upgrade`）。

## 端口

| 方向 | 端口 | 用途 |
|---|---|---|
| 入站 | 443/tcp | WebUI、webhook、REST、MCP |
| 入站（可选） | 443/udp | HTTP/3 |
| 入站（可选） | 80/tcp | ACME HTTP-01 与跳转 HTTPS；不开时 Caddy 用 443 上的 TLS-ALPN-01 签证书 |
| 出站 | 443/tcp | R2、Telegram、ACME、拉取镜像 |
| 出站 | 53 | DNS |
| 出站 | 123/udp | NTP：webhook 签名带时间窗口（§2.2），时钟必须准 |
| 出站（M2 起） | 465/tcp | 经 Cloudflare Email Sending 发信 |
| 无 | 25 | 收信由 Cloudflare Email Routing 完成，MX 不指向这台机器 |

M2 的 SMTP submission 口（§13.6）只给同一部署环境里的服务用，不对公网开放。

## 本地试跑

[`compose.local.yaml`](compose.local.yaml) 把 Caddy 换到本机回环的 31443/31408 端口，用 Caddy 自己的内部 CA 签 `localhost`：

```bash
docker build -t yuuka:local ..
# .env 中：YUUKA_IMAGE=yuuka:local，YUUKA_WEB_ORIGIN=https://localhost:31443
docker compose -f compose.yaml -f compose.local.yaml up -d
```
