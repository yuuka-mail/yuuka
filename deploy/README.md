# 用 Docker Compose 部署

一台有固定公网地址的机器上跑整套 yuuka：yuuka 本体，Postgres，定时 `pg_dump` 的备份服务，
以及前面终结 TLS 的反向代理。这只是部署方式之一；yuuka 本身不假设部署形态（§2），
每个端点自己认证（§13.7），代理不是安全边界。

| 服务 | 作用 | 网络 | 卷 |
|---|---|---|---|
| `caddy` | 仅「自带 Caddy」模式（profile `caddy`）：发布 80/443，自动申请和续期证书 | `edge` | `caddy-data`、`caddy-config` |
| `migrate` | 一次性的 `yuuka migrate`：以 `yuuka_owner` 应用迁移后退出；`yuuka` 等它成功才启动 | `backend` | 无 |
| `yuuka` | `yuuka serve`：webhook、reconcile、WebUI、REST、MCP | `edge` + `backend` | `yuuka-data`（本地 raw 副本） |
| `postgres` | 官方镜像，按 digest 固定；首次初始化时建角色（[`postgres-roles.sh`](postgres-roles.sh)） | `backend` | `postgres-data` |
| `backup` | 定时 `pg_dump`，见 [`backup.sh`](backup.sh) | `backend` | `backups` |

`backend` 是 `internal` 网络，没有出口也不可从外部进入。yuuka 在容器网络里监听 31480
（`YUUKA_LISTEN=0.0.0.0:31480`）；只有「宿主机代理」模式才把它发布到宿主机，而且只在 `127.0.0.1` 上。

## 两种模式

`.env` 里二选一：

- **自带 Caddy**：`COMPOSE_PROFILES=caddy`。`caddy` 服务占 80/443，配置是 [`Caddyfile`](Caddyfile)。
  适合这台机器上没有别的 Web 服务
- **宿主机已有反向代理**（例如 systemd 跑的 Caddy 已经占了 80/443）：不开 profile，写
  `COMPOSE_FILE=compose.yaml:compose.host-caddy.yaml`。[`compose.host-caddy.yaml`](compose.host-caddy.yaml)
  把 yuuka 发布到 `127.0.0.1:31480`（端口可用 `YUUKA_HOST_PORT` 改），绝不在公网网卡上监听；
  宿主机的 Caddyfile 里加上 [`Caddyfile.host`](Caddyfile.host) 的内容，站点名换成 `YUUKA_WEB_ORIGIN` 的主机名

两种模式下 `docker compose up -d`、`docker compose run …` 都照常用，`.env` 决定起哪些服务。

## 首次启动

前提：Docker Engine 与 compose 插件；`YUUKA_WEB_ORIGIN` 的主机名（如 `mail.example.com`）已解析到本机。

```bash
# 把 deploy/ 目录放到服务器上，然后在其中：
cp compose.env.example .env        # 模式、镜像、WebUI origin、Postgres 密码、备份策略
cp yuuka.env.example yuuka.env     # R2、webhook 密钥、Telegram……
chmod 600 .env yuuka.env
$EDITOR .env yuuka.env
docker compose up -d               # 先跑 migrate 建表，成功后才启动 yuuka
docker compose logs migrate        # schema version 0 -> N
docker compose logs -f yuuka       # 看到 listening 即可
```

发布版本之前没有现成镜像，在仓库根目录自己构建，并在 `.env` 里写 `YUUKA_IMAGE=yuuka:local`：

```bash
docker build --build-arg VERSION=$(git describe --tags --always) -t yuuka:local .
```

Email Worker 的 webhook 地址是 `<YUUKA_WEB_ORIGIN>/ingest/cloudflare`，密钥与 `YUUKA_WEBHOOK_SECRETS` 一致。
这个路径必须能从 Cloudflare 访问，不要在它前面加 IP 白名单或浏览器登录（见 [`Caddyfile`](Caddyfile)）。

## R2 令牌

yuuka 用两把 R2 API 令牌（S3 兼容的 Access Key ID / Secret），各自只限定到一个 bucket（§2.2）：

| 变量前缀 | bucket | 权限 | yuuka 用它做什么 |
|---|---|---|---|
| `YUUKA_R2_RAW_` | raw | Object Read & Write | 读 Worker 写下的 `raw/` 原件；只新建、从不覆盖 `sent/` 下的 Sent 副本 |
| `YUUKA_R2_PENDING_` | pending | Object Read & Write | 读、列、删投递标记 |

raw 令牌要写权限，是因为发出的信（Sent 副本）也存在 raw bucket 的 `sent/` 前缀下。R2 的令牌管不到前缀，
所以「`raw/` 下的原件不可变」靠 yuuka 的代码守住：它只往 `sent/` 写、每次写都是「不存在才写」，没有删除操作。
这把令牌泄露时，持有者能改写或删除 `raw/` 下的原件，按对待 pending 令牌的方式保管它，不要给 yuuka 以外的进程。
想在权限层面再加一道，可以给 raw bucket 的 `raw/`、`sent/` 前缀加 R2 bucket lock 规则（阻止删除和覆盖，不阻止新建）；
代价是保留期内谁都删不掉这些对象，取舍见 §2.2。

早先的部署给 raw bucket 的是 Object Read only 令牌。只收信时它仍然够用；启用发信（M2）之前换成 Object Read & Write，
否则 Sent 副本写不进去。

## 数据库角色

yuuka 不以超级用户连库。[`postgres-roles.sh`](postgres-roles.sh) 在 Postgres 第一次初始化空数据目录时建三个角色，
理由见 §4「数据库角色」：

| 角色 | 谁用 | 权限 |
|---|---|---|
| `postgres` | 只有初始化和人工维护 | 超级用户；没有服务用它连库 |
| `yuuka_owner` | 只有一次性的 `migrate` 服务（`YUUKA_DATABASE_OWNER_URL`） | 拥有库和 `public` schema；装 `pg_trgm`（trusted 扩展） |
| `yuuka_app` | `yuuka serve` 和所有管理命令（`YUUKA_DATABASE_URL`） | 表上增删改查、序列 `USAGE`；没有任何 DDL，不能建临时表 |
| `yuuka_backup` | 备份服务 | `pg_read_all_data`：只读 |

四个密码都在 `.env`，用 `openssl rand -hex 32` 生成（要拼进连接串，所以用十六进制）。
连接串由 `compose.yaml` 拼好：owner 的只给 `migrate`，app 的只给 `yuuka`，`yuuka.env` 里不用写。
常驻的 `yuuka` 进程环境里没有 owner 的凭据（`YUUKA_DATABASE_OWNER_URL` 被强制设为空，`yuuka.env` 里写了也不生效），
拿到这个进程的人改不了表结构。

脚本可以重复跑，作用是把权限修回应有的样子（恢复之后要跑一次，见下文）：

```bash
docker compose exec postgres sh /docker-entrypoint-initdb.d/10-yuuka-roles.sh
```

## 客户端地址

yuuka 默认不看 `X-Forwarded-For`。在代理后面这意味着所有人看起来都是代理：
登录限速成了一个共享的桶（谁都能刷爆它把你挡在外面），审计也只记到代理。所以 `compose.yaml` 让 yuuka
信任 `edge` 网络（`YUUKA_TRUSTED_PROXIES`）：自带的 Caddy 在这个网络里，宿主机进程经回环端口进来时
来自它的网关。只有对端可信时 yuuka 才读这个头，从右往左跳过可信的代理，取第一个不可信的地址（§13.7）。

- `edge` 的网段固定为 `10.213.77.0/24`，与宿主机上的网络冲突时在 `.env` 里改 `YUUKA_EDGE_SUBNET`
- 宿主机代理模式下，宿主机上任何能连 `127.0.0.1:31480` 的进程都会被当成代理；这台机器上的进程本来就在信任范围内
- **代理前面还有 Cloudflare 代理（或别的 CDN）时**，两处都要配：Caddy 的 `trusted_proxies` 写 CDN 的网段
  （[`Caddyfile.host`](Caddyfile.host) 里有一份 Cloudflare 的静态清单），否则 Caddy 会把 CDN 节点当成客户端；
  `.env` 的 `YUUKA_EXTRA_TRUSTED_PROXIES` 写同一份网段（逗号分隔），否则 yuuka 会停在 CDN 节点上。
  清单是抄下来的，隔一阵对照 <https://www.cloudflare.com/ips/> 更新。客户端直连代理时两处都不要配：
  信任一个没人经过的网段，只会让那里的人能自报地址
- 不用 Compose、直接在宿主机上跑 `yuuka serve` 时，`YUUKA_TRUSTED_PROXIES=127.0.0.1/32,::1/128`

## 健康检查

`GET /healthz` 不需要认证：能在短超时内 ping 通 Postgres 就是 200，否则 503，不透露别的（§13.7）。
两种模式的 Caddyfile 都用它做主动健康检查（`health_uri /healthz`）；不健康时 Caddy 直接回 503。

`yuuka` 服务**没有** Docker 层面的 `HEALTHCHECK`：镜像是 distroless，没有 shell 也没有 curl，
要做只能给 yuuka 加一个探测用的子命令，目前没有。`docker compose ps` 里 yuuka 因此不显示健康状态，
看代理的日志或者 `curl https://<域名>/healthz`。

## 管理命令

管理命令在主机上用 `docker compose run` 跑，复用 `yuuka` 服务的环境，即只有 app 角色；能跑它们就是最高权限（§13.7）。
`run` 会先把依赖拉起来，所以每次也会顺带跑一遍 `migrate`，库已是最新时它只打印 `up to date`。

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

Postgres 里有无法从原件重建的数据（§4.2、§7），备份是必需的。`backup` 服务以只读的 `yuuka_backup` 每
`BACKUP_INTERVAL` 秒（默认 86400，即每天 00:00 UTC）做一次 `pg_dump --format=custom`，
确认 `pg_restore --list` 能读回后才落成 `yuuka-<UTC 时间>.dump`，并只保留最新的 `BACKUP_KEEP` 个（默认 14）。
按个数而不是按天数清理：备份连续失败时旧的不会被逐个过期删光。超过两个周期没有新备份时，
`docker compose ps` 里 `backup` 显示 `unhealthy`。备份服务用只读角色而不是 owner：
它是唯一无人值守、按定时器跑的容器，被攻破也只能读。

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

恢复到一个空库，以 `yuuka_owner` 身份导入（经 Postgres 容器的本地 socket，不需要密码），
导完重跑一次角色脚本，把库级权限和默认权限修回来。原地回滚时先删库重建；
灾难恢复时就是新机器上 `docker compose up -d postgres` 得到的新卷（角色已由初始化建好）。

```bash
docker compose stop yuuka backup
docker compose exec postgres dropdb -U postgres --force yuuka               # 新机器上跳过这一步
docker compose exec postgres createdb -U postgres -O yuuka_owner yuuka      # 新机器上跳过这一步
# 备份卷里的 dump：
docker compose run --rm --no-deps -T --entrypoint cat backup /backups/yuuka-20250101T000000Z.dump |
  docker compose exec -T postgres pg_restore -U yuuka_owner -d yuuka --no-owner --exit-on-error --single-transaction
# 或者从机器外拷回来的 dump：
docker compose exec -T postgres pg_restore -U yuuka_owner -d yuuka --no-owner --exit-on-error --single-transaction < yuuka.dump
docker compose exec postgres sh /docker-entrypoint-initdb.d/10-yuuka-roles.sh
docker compose up -d
```

`--single-transaction` 让恢复要么全部成功、要么什么都不改。dump 来自旧版本时，最后的 `docker compose up -d`
会先由 `migrate` 把表结构补到当前版本。恢复后，备份时刻之后到达的邮件仍在 R2 里，
`docker compose run --rm yuuka reconcile --full` 会把它们补回来。**定期演练一次恢复**，没恢复过的备份不算备份。

## 从单角色部署迁移

早期的模板让 yuuka 以 Postgres 镜像建的超级用户 `yuuka` 连库。超级用户建的对象不能整体转给别的角色，
所以迁移就是「备份、换新卷、恢复」：

1. 旧配置下备份一次并拷出机器：`docker compose exec backup sh /usr/local/bin/yuuka-backup once`，再按上文取出 dump
2. `docker compose down`，然后 `docker volume rm yuuka_postgres-data`（**先确认 dump 在手**）
3. 换上新的 `compose.yaml`、`postgres-roles.sh`，在 `.env` 里补 `YUUKA_DB_OWNER_PASSWORD`、`YUUKA_DB_APP_PASSWORD`、
   `YUUKA_DB_BACKUP_PASSWORD` 和模式设置；`POSTGRES_PASSWORD` 现在只属于超级用户 `postgres`
4. `docker compose up -d postgres`，按上一节从机器外的 dump 恢复，再 `docker compose up -d`

## 升级

镜像在打版本 tag 时发布到 `ghcr.io/yuuka-mail/yuuka:<版本>`。升级就是改 `.env` 里的
`YUUKA_IMAGE`（建议连 digest 一起写），然后：

```bash
docker compose exec backup sh /usr/local/bin/yuuka-backup once   # 先备份
docker compose pull migrate yuuka      # 或者 docker load 本地构建的镜像
docker compose up -d                   # migrate 先跑完，yuuka 才以新镜像重启
docker compose logs migrate            # schema version 4 -> 5，或 up to date
```

`yuuka serve` 自己不迁移：它只有 app 角色，启动时只核对库的版本。迁移只追加，由 `migrate` 服务以 `yuuka_owner` 执行；
它失败时 `docker compose up -d` 报 `service "migrate" didn't complete successfully`，
不会（重新）启动 `yuuka`，原因看 `docker compose logs migrate`。也可以单独手动跑，可重复执行，两个同时跑也安全：

```bash
docker compose run --rm migrate
```

绕过 `migrate` 直接用新镜像起 `serve`（例如 `docker compose up -d --no-deps yuuka`）时，它拒绝启动，
容器按 `restart` 策略反复退出，日志是：

```
yuuka: database is at version 4, this binary needs 5: run `yuuka migrate` as the schema owner (YUUKA_DATABASE_OWNER_URL), then start again
```

跑一次 `docker compose run --rm migrate`（或直接 `docker compose up -d`）即可。反过来，库被更新的版本迁移过时
（回退了镜像），`serve` 和 `migrate` 都拒绝：`database is at version 5, this binary only knows 4`。
旧版本不认识新表结构，回退版本要连同升级前的备份一起恢复。
Caddy、Postgres 的镜像 digest 写在 `compose.yaml` 里，随仓库更新。Postgres 跨大版本不能直接换镜像，
要 `pg_dump` 后恢复到新版本的空卷（或 `pg_upgrade`）。

## 端口

| 方向 | 端口 | 用途 |
|---|---|---|
| 入站 | 443/tcp | WebUI、webhook、REST、MCP |
| 入站（可选） | 443/udp | HTTP/3 |
| 入站（可选） | 80/tcp | ACME HTTP-01 与跳转 HTTPS；不开时 Caddy 用 443 上的 TLS-ALPN-01 签证书 |
| 仅本机 | 127.0.0.1:31480/tcp | 宿主机代理模式下 yuuka 本身 |
| 出站 | 443/tcp | R2、Telegram、ACME、拉取镜像 |
| 出站 | 53 | DNS |
| 出站 | 123/udp | NTP：webhook 签名带时间窗口（§2.2），时钟必须准 |
| 出站（M2 起） | 465/tcp | 经 Cloudflare Email Sending 发信 |
| 无 | 25 | 收信由 Cloudflare Email Routing 完成，MX 不指向这台机器 |

M2 的 SMTP submission 口（§13.6）只给同一部署环境里的服务用，不对公网开放。

## 本地试跑

[`compose.local.yaml`](compose.local.yaml) 把自带 Caddy 换到本机回环的 31443/31408 端口，用 Caddy 自己的内部 CA 签 `localhost`：

```bash
docker build -t yuuka:local ..
# .env 中：COMPOSE_PROFILES=caddy，YUUKA_IMAGE=yuuka:local，YUUKA_WEB_ORIGIN=https://localhost:31443
docker compose -f compose.yaml -f compose.local.yaml up -d
```
