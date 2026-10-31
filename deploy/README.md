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

## 发信

出口（transport）按名字写在 `yuuka.env` 里（§10.2）：`YUUKA_TRANSPORTS` 列名字，每个名字一组
`YUUKA_TRANSPORT_<名字>_HOST` / `_PORT` / `_USERNAME` / `_PASSWORD_FILE` / `_CAPS`，模板里有 Cloudflare 的例子。
identity 只引用名字；名字在配置里找不到时信留在队列里不发，日志报错（§4.5）。

SMTP 密码只放在文件里，变量写的是容器里的路径。用 Compose 的 secret 挂进去，写在
`compose.override.yaml`（和 `compose.yaml` 放一起，Compose 会自动叠加；用了 `COMPOSE_FILE` 时把它加进列表）：

```yaml
services:
  yuuka:
    secrets: [cloudflare-smtp]
secrets:
  cloudflare-smtp:
    file: ./secrets/cloudflare-smtp   # secrets/ 和这个文件都在 .gitignore 里
```

文件型 secret 是原样绑定挂载进去的，宿主机上的属主和权限会带进容器；镜像以 `65532:65532`（distroless 的 nonroot）运行，
所以文件要归它，只给 `chmod 600` 而属主是 root 时，yuuka 启动时读不到密码：

```bash
install -d -m 700 secrets
install -m 400 -o 65532 -g 65532 /path/to/smtp-password secrets/cloudflare-smtp
```

容器里的路径是 `/run/secrets/cloudflare-smtp`。文件只在启动时读，换密码后 `docker compose up -d yuuka`。
凭据失效（`AUTH` 被拒）、连不上出口时，yuuka 暂停这个出口而不是把信判死，信留在队列里，
出口恢复后（换了密码则在重启后）接着发；在 `docker compose logs yuuka` 里找 `transport paused`。

```bash
docker compose run --rm yuuka outbox ls                     # 队列、状态、最后的错误
docker compose run --rm yuuka outbox ls --state unknown     # 可能已发出、等人决定的信
docker compose run --rm yuuka outbox resolve <id> --sent    # 或 --resend / --abandon（§4.5）
```

停止或重启 `yuuka` 时，已经越过提交点的发送会等服务器的最终回复，所以 `compose.yaml` 给了 1 分钟的停止宽限期。

## SMTP 提交口

让同一部署环境里的服务（监控、告警、定时任务）不改代码、用 SMTP 直接发信（§13.6）。默认关闭，
`YUUKA_SUBMISSION_LISTEN` 打开它。只做隐式 TLS（465 的形式），不做 STARTTLS。

### 先选暴露范围

提交口自己认证（§13.7），放在哪里可达是部署的选择。先弄清两件事：

- **Cloudflare 的代理保护不了它**。橙色云只代理 HTTP(S)，任意 TCP 端口要 Spectrum（付费）。
  给提交口配的主机名只能是灰云记录，直接解析到接收连接的那台机器
- **在 yuuka 这台机器上开公网提交口，等于公开这台机器的 IP**。如果 Web 和 webhook 靠 Cloudflare 代理藏住源站，
  这个 IP 一公开，攻击者就能绕过 Cloudflare 直接打它的 443。yuuka 的连接上限和认证失败限速挡得住暴力破解，
  挡不住把带宽打满的流量型攻击——那时同一台机器上的所有端口一起受影响

所以按发信方在哪里，三选一：

| 做法 | 适合 | 代价 |
|---|---|---|
| **只在私有网络可达**（推荐的默认） | 发信方都是自己的服务，能进同一个内网、tailnet 或 WireGuard | 发信方得能加入那个私有网络 |
| **公网经独立中转机** | 有发信方在私有网络外（第三方云服务、不归你管的机器） | 多一台机器；它的 IP 公开，被打时只有提交口受影响 |
| **公网、同一台机器** | 这台机器本来就直接暴露在公网、没有 CDN 在前面要保护 | 源站 IP 公开；流量攻击连带 Web 和 webhook |

被打期间不会丢信：webhook 连不上时 Cloudflare 照常收，Worker 把原件写进 R2、留下待处理标记，
恢复后 reconcile 补进来（§2.1），只是推迟；已进 outbox 的信照常按状态机重试（§4.5）。

不论哪种，下面的「yuuka 自己终结 TLS」「代理终结 TLS」都适用，区别只在端口发布在哪个地址、PROXY 头由谁加。

**只在私有网络可达**：端口只发布在内网 / tailnet / WireGuard 网卡的地址上，不建公网 DNS 记录。
客户端用的主机名解析到这个私有地址即可（tailnet 的 MagicDNS、内网 DNS，或客户端的 hosts）。

**公网经独立中转机**：一台小 VPS 只做 TCP 转发，经 WireGuard 等私有链路把连接交给 yuuka，并加 PROXY v2 头。
建议中转机**不终结 TLS**，原样转发密文、由 yuuka 终结：证书私钥和客户端的 SMTP 密码都不经过这台最暴露、
最容易被替换的机器，它被攻破也只能看到密文。提交口的主机名（如 `submit.example.com`）解析到中转机的公网 IP；
它的证书要用 DNS-01 签——这个名字的 HTTP-01 验证打到的是中转机，不是 yuuka。

```
# 中转机上的 HAProxy：只转发，不解 TLS
frontend submission
    mode tcp
    bind :465
    maxconn 256                     # 中转机自己的上限，先于 yuuka 的连接上限挡掉洪峰
    timeout client 5m
    default_backend yuuka_submission

backend yuuka_submission
    mode tcp
    timeout server 5m
    server yuuka 10.8.0.1:31465 send-proxy-v2   # yuuka 在私有链路上的地址
```

```bash
# yuuka.env：yuuka 终结 TLS，只认中转机发来的 PROXY 头
YUUKA_SUBMISSION_LISTEN=0.0.0.0:31465
YUUKA_SUBMISSION_TLS_CERT_FILE=/certs-src/live/submit.example.com/fullchain.pem
YUUKA_SUBMISSION_TLS_KEY_FILE=/certs-src/live/submit.example.com/privkey.pem
YUUKA_SUBMISSION_PROXIES=10.8.0.2/32   # 中转机在私有链路上的地址
```

```yaml
# compose.override.yaml：只发布在私有链路的地址上
services:
  yuuka:
    ports:
      - "10.8.0.1:31465:31465"
```

`YUUKA_SUBMISSION_PROXIES` 写的是 **yuuka 看到的**来源地址。经 Docker 的端口发布进来时，外部网卡上的连接通常保留源地址，
经回环进来时看到的是 Compose 网络的网关（见下文「代理终结 TLS」的例子）；拿不准就先启动、试连一次，
在日志或 `actions` 的 `smtp.auth.reject` 里看 yuuka 记下的地址。中转机被打或被封，换一台、改 DNS 即可，yuuka 这边只改这一行。

**公网、同一台机器**：端口发布在公网网卡上（`"465:31465"`），主机名直接解析到这台机器。只在上表的前提下这样做；
如果这台机器的 Web 本来靠 Cloudflare 代理，选前两种之一。

顺带一提，靠 Cloudflare 代理保护 Web 时，源站的 443 最好只放行 Cloudflare 的 IP 段（<https://www.cloudflare.com/ips/>），
或改用 cloudflared tunnel 完全不开入站端口；否则源站 IP 一旦被扫到或从历史 DNS 记录里查到，
Cloudflare 那一层就被绕过了。这和提交口无关，但前面那张表的取舍以它为前提。

### 凭据与客户端

每个服务一把 SMTP 密码，用户名是命令单独打印的凭据 id；用哪个发件地址、发到哪一步由 `yuuka identity` 和 `yuuka grant` 决定（§13.5）：

```bash
docker compose run --rm yuuka identity add alerts@example.com --owner service:monitoring --transport cloudflare
docker compose run --rm yuuka grant set --identity alerts@example.com --principal service:monitoring \
  --policy auto --recipients allowlist --allow ops@example.com
docker compose run --rm yuuka token issue --kind smtp --principal service:monitoring --label alertmanager
```

客户端配置：主机名是证书上的名字（如 `mail.example.com`），端口 465，「SSL/TLS」或「implicit TLS」（不是 STARTTLS），
AUTH PLAIN。停在草稿或待审核的信同样回 `250`，回复里带 outbox / 草稿 id；被策略拒绝回 `550 5.7.1` 和原因。

### TLS 由谁终结

容器里 yuuka 以非 root 运行、没有任何 capability，所以在容器里监听 31465，由 Compose 或代理映射到 465。
两种方式二选一；下面的例子按「只在私有网络可达」写，端口发布的地址按上文所选的暴露范围改。

**yuuka 自己终结 TLS**：给它证书和私钥的 PEM 文件。来源随意（certbot、宿主机 Caddy 的证书目录……）；
文件变了 yuuka 一分钟内换上新证书，续期不用重启。挂载**目录**而不是单个文件：续期多半是换文件或改符号链接，
单文件的 bind mount 会一直指着旧的那个。私钥要让容器里的用户（distroless 的 `nonroot`，uid 65532）读得到。

```bash
# yuuka.env
YUUKA_SUBMISSION_LISTEN=0.0.0.0:31465
YUUKA_SUBMISSION_TLS_CERT_FILE=/certs/mail.example.com/fullchain.pem
YUUKA_SUBMISSION_TLS_KEY_FILE=/certs/mail.example.com/privkey.pem
```

```yaml
# compose.override.yaml
services:
  yuuka:
    ports:
      # 只发布在内网 / tailnet 网卡的地址上（或 127.0.0.1），不要省略地址：那等于 0.0.0.0
      - "127.0.0.1:465:31465"
    volumes:
      - /etc/letsencrypt:/certs-src:ro   # 示意；路径按证书的实际位置改
```

（certbot 的 `live/` 是指向 `archive/` 的符号链接，挂整个 `/etc/letsencrypt`，变量写 `/certs-src/live/<名字>/fullchain.pem`。）

**代理终结 TLS**：代理在 465 上解开 TLS，以明文转给 yuuka 的 31465，并且**必须**带 PROXY protocol 头（v1 或 v2），
否则限速和审计里的客户端全是代理自己。`YUUKA_SUBMISSION_PROXIES` 列出代理的地址，只有它们能发明文、必须发 PROXY 头；
这和 HTTP 的 `YUUKA_TRUSTED_PROXIES` 是两份名单（§13.6）。没有证书文件又没有这份名单时 yuuka 拒绝启动。

```bash
# yuuka.env；宿主机上的代理经回环端口进来时来自 edge 网络的网关
YUUKA_SUBMISSION_LISTEN=0.0.0.0:31465
YUUKA_SUBMISSION_PROXIES=10.213.77.0/24
```

```yaml
# compose.override.yaml：只发布给宿主机上的代理
services:
  yuuka:
    ports:
      - "127.0.0.1:31465:31465"
```

HAProxy：

```
frontend submission
    mode tcp
    bind 127.0.0.1:465 ssl crt /etc/haproxy/certs/mail.example.com.pem   # 内网 / tailnet 地址
    default_backend yuuka_submission

backend yuuka_submission
    mode tcp
    server yuuka 127.0.0.1:31465 send-proxy-v2
```

Caddy 要带 [caddy-l4](https://github.com/mholt/caddy-l4) 模块的构建（官方镜像没有），在 `layer4` 里用 `tls` 终结、
`proxy` 的 `proxy_protocol v2` 转给 `127.0.0.1:31465`；Traefik 的 TCP router 加 `tls` 和服务上的 `proxyProtocol`。
无论哪种，代理健康检查用 PROXY v2 的 `LOCAL` 或 v1 的 `UNKNOWN`。代价：从代理那个地址直连 31465 的其他进程
（比如同一台宿主机上的服务）不带 PROXY 头会被断开，它们也要经过代理。

### 审计

在 `actions` 里看提交口：`smtp.auth`、`smtp.auth.reject`（带原因）、`smtp.submit`（回复码与 outbox / 草稿 id），
都带客户端地址和经过的代理。撤销一把密码（`yuuka token revoke <凭据 id>`）对开着的会话也立即生效。

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
| 入站（可选） | 465/tcp | SMTP 提交口：默认只发布在内网 / tailnet 地址上；公网可达时经独立中转机（见「先选暴露范围」） |
| 无 | 25 | 收信由 Cloudflare Email Routing 完成，MX 不指向这台机器 |

## 本地试跑

[`compose.local.yaml`](compose.local.yaml) 把自带 Caddy 换到本机回环的 31443/31408 端口，用 Caddy 自己的内部 CA 签 `localhost`：

```bash
docker build -t yuuka:local ..
# .env 中：COMPOSE_PROFILES=caddy，YUUKA_IMAGE=yuuka:local，YUUKA_WEB_ORIGIN=https://localhost:31443
docker compose -f compose.yaml -f compose.local.yaml up -d
```
