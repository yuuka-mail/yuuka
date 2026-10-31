# SMTP 提交口

让同一部署环境里的服务（监控、告警、定时任务）不改代码、用 SMTP 经 yuuka 发信。默认关闭，
`YUUKA_SUBMISSION_LISTEN` 打开它。只做隐式 TLS（465 的形式），不做 STARTTLS。
这一页讲网络上怎么摆，和用什么方式部署无关；用 Docker Compose 时端口怎么发布见
[deploy/README.md](../../deploy/README.md#smtp-提交口)，给服务发凭据、客户端怎么配见[接入 SMTP](../integrate/smtp.md)。

为什么只做隐式 TLS、为什么必须用 PROXY protocol：[设计](../design/principals.md#136-smtp-submission让其他服务直接发信)。

## 先选暴露范围

提交口自己认证，放在哪里可达是部署的选择。先弄清两件事：

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

被打期间不会丢信：webhook 连不上时 Cloudflare 照常收，Worker 把原件写进 R2、留下投递标记，
恢复后 yuuka 补进来，只是推迟；已经进了发件队列的信照常重试。

不论哪种，下面「TLS 由谁终结」的两种方式都适用，区别只在端口监听在哪个地址、PROXY 头由谁加。

**只在私有网络可达**：端口只监听或发布在内网 / tailnet / WireGuard 网卡的地址上，不建公网 DNS 记录。
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
# yuuka 的配置：yuuka 终结 TLS，只认中转机发来的 PROXY 头
YUUKA_SUBMISSION_LISTEN=10.8.0.1:31465
YUUKA_SUBMISSION_TLS_CERT_FILE=/etc/letsencrypt/live/submit.example.com/fullchain.pem
YUUKA_SUBMISSION_TLS_KEY_FILE=/etc/letsencrypt/live/submit.example.com/privkey.pem
YUUKA_SUBMISSION_PROXIES=10.8.0.2/32   # 中转机在私有链路上的地址
```

`YUUKA_SUBMISSION_PROXIES` 写的是 **yuuka 看到的**来源地址。中间隔着容器网络、NAT 时它可能不是中转机自己的地址；
拿不准就先启动、试连一次，在日志或审计的 `smtp.auth.reject` 里看 yuuka 记下的地址。
中转机被打或被封，换一台、改 DNS 即可，yuuka 这边只改这一行。

**公网、同一台机器**：端口监听在公网网卡上，主机名直接解析到这台机器。只在上表的前提下这样做；
如果这台机器的 Web 本来靠 Cloudflare 代理，选前两种之一。

顺带一提，靠 Cloudflare 代理保护 Web 时，源站的 443 最好只放行 Cloudflare 的 IP 段（<https://www.cloudflare.com/ips/>），
或改用 cloudflared tunnel 完全不开入站端口；否则源站 IP 一旦被扫到或从历史 DNS 记录里查到，
Cloudflare 那一层就被绕过了。这和提交口无关，但前面那张表的取舍以它为前提。

## TLS 由谁终结

二选一。

**yuuka 自己终结 TLS**：给它证书和私钥的 PEM 文件。来源随意（certbot、反向代理的证书目录……）；
文件变了 yuuka 一分钟内换上新证书，续期不用重启，新文件读不出来时继续用旧的。私钥要让运行 yuuka 的用户读得到。

```bash
YUUKA_SUBMISSION_LISTEN=10.0.0.5:465
YUUKA_SUBMISSION_TLS_CERT_FILE=/etc/letsencrypt/live/mail.example.com/fullchain.pem
YUUKA_SUBMISSION_TLS_KEY_FILE=/etc/letsencrypt/live/mail.example.com/privkey.pem
```

**代理终结 TLS**：代理在 465 上解开 TLS，以明文转给 yuuka，并且**必须**带 PROXY protocol 头（v1 或 v2），
否则限速和审计里的客户端全是代理自己。`YUUKA_SUBMISSION_PROXIES` 列出代理的地址，只有它们能发明文、必须发 PROXY 头；
这和 HTTP 的 `YUUKA_TRUSTED_PROXIES` 是两份名单。没有证书文件又没有这份名单时 yuuka 拒绝启动。

```bash
YUUKA_SUBMISSION_LISTEN=127.0.0.1:31465
YUUKA_SUBMISSION_PROXIES=127.0.0.1/32
YUUKA_SUBMISSION_HOSTNAME=mail.example.com   # yuuka 看不到证书，问候里报的名字要写上
```

HAProxy：

```
frontend submission
    mode tcp
    bind 10.0.0.5:465 ssl crt /etc/haproxy/certs/mail.example.com.pem   # 内网 / tailnet 地址
    default_backend yuuka_submission

backend yuuka_submission
    mode tcp
    server yuuka 127.0.0.1:31465 send-proxy-v2
```

Caddy 要带 [caddy-l4](https://github.com/mholt/caddy-l4) 模块的构建（官方镜像没有），在 `layer4` 里用 `tls` 终结、
`proxy` 的 `proxy_protocol v2` 转给 yuuka；Traefik 的 TCP router 加 `tls` 和服务上的 `proxyProtocol`。
无论哪种，代理的健康检查用 PROXY v2 的 `LOCAL` 或 v1 的 `UNKNOWN`。代价：和代理同一个地址、直连 yuuka 这个端口的其他进程
不带 PROXY 头会被断开，它们也要经过代理。

## 审计

审计（`actions`）里能看到提交口的每一步：`smtp.auth`、`smtp.auth.reject`（带原因）、`smtp.submit`（回复码与发件队列 / 草稿 id），
都带客户端地址和经过的代理，从不记密码和正文。撤销一把密码（`yuuka token revoke <凭据 id>`）对开着的会话也立即生效。
