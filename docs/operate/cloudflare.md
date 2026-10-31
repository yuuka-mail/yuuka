# Cloudflare 侧的配置

yuuka 用 Cloudflare 做三件事：Email Routing 收信并交给一个 Email Worker，R2 存原件和投递标记，
Email Sending 发信。这一页按上线顺序写这三件事要配什么。yuuka 本体怎么跑见 [deploy/README.md](../../deploy/README.md)。

## R2：两个 bucket、两把令牌

建两个 bucket，名字随意但必须不同，例如 `yuuka-raw` 和 `yuuka-pending`：

| bucket | 放什么 |
|---|---|
| raw | 收到的原件 `raw/YYYY/MM/<id>.eml`，以及 yuuka 发出的信的副本 `sent/YYYY/MM/<id>.eml`。写一次永不改 |
| pending | Worker 留下的投递标记 `pending/<id>`，yuuka 入库后删掉 |

Worker 通过绑定访问这两个 bucket，不需要令牌。yuuka 用两把 R2 API 令牌（S3 兼容的 Access Key ID / Secret），
**各自只限定到一个 bucket**：

| 配置项前缀 | bucket | 权限 | yuuka 用它做什么 |
|---|---|---|---|
| `YUUKA_R2_RAW_` | raw | Object Read & Write | 读 Worker 写下的原件；只新建、从不覆盖 `sent/` 下的副本 |
| `YUUKA_R2_PENDING_` | pending | Object Read & Write | 读、列、删投递标记 |

S3 端点是 `https://<account id>.r2.cloudflarestorage.com`，写进 `YUUKA_R2_ENDPOINT`。

raw 令牌要写权限，是因为发出的信也存在 raw bucket 的 `sent/` 下。R2 的令牌管不到前缀，
所以「原件不可变」靠 yuuka 的代码守住：它只往 `sent/` 写、每次都是「不存在才写」，没有删除操作。
这把令牌泄露时，持有者能改写或删除原件，**只给 yuuka，按最敏感的凭据保管**。
想在权限层面再加一道，可以给 raw bucket 的 `raw/`、`sent/` 前缀加 R2 bucket lock 规则（阻止删除和覆盖，不阻止新建）；
代价是保留期内谁都删不掉这些对象（[取舍](../design/architecture.md#22-worker--go-契约v1)）。

早先的部署给 raw bucket 的是 Object Read only 令牌。只收信时它仍然够用；启用发信之前换成 Object Read & Write，
否则发出的信的副本写不进去，信发不出去。

## Email Worker：收信

Worker 的代码在 [`worker/`](../../worker/)。每封信它先把原件写进 R2、留下投递标记，再调用 yuuka 的 webhook。
webhook 调不通也不要紧：yuuka 每隔几分钟扫一遍投递标记，把漏掉的补进来。所以 yuuka 停机期间不会丢信，只是推迟。

部署一次：

```bash
mise install && mise run js:install    # 在仓库根目录：装好固定版本的 Node、pnpm 和 wrangler
cd worker
cp wrangler.example.toml wrangler.toml # wrangler.toml 被 gitignore，真实值只写在这里
$EDITOR wrangler.toml
pnpm exec wrangler secret put WEBHOOK_SECRET
pnpm exec wrangler deploy
```

`wrangler.toml` 里要改的：

- `INGEST_URL`：`<YUUKA_WEB_ORIGIN>/ingest/cloudflare`。这个路径必须能从 Cloudflare 访问，
  不要在它前面加 IP 白名单或浏览器登录；它自己用签名认证
- 两个 `bucket_name`：上面建的 bucket。绑定名 `RAW`、`PENDING` 不要改，代码按这两个名字找
- `addresses = ["*@<你的域>"]`：把整个域的 catch-all 指给这个 Worker。只能写顶级域（zone 本身），
  需要 Wrangler 4.113 以上；`wrangler email routing rules update … catch-all` 只能 forward / drop，指不到 Worker。
  也可以在 dashboard 里把 catch-all 规则的动作设成这个 Worker

`WEBHOOK_SECRET` 用 `openssl rand -hex 32` 生成，同一个值写进 yuuka 的 `YUUKA_WEBHOOK_SECRETS`。
**换密钥**时不停机：先在 yuuka 那边写成「新,旧」两个并重启，再 `wrangler secret put` 换 Worker 的，
确认新的生效后把旧的从 yuuka 里删掉。

### 接入一个新域

1. 在这个 zone 上启用 Email Routing（Cloudflare 会提示添加它的 MX 和 SPF 记录）
2. 在 `wrangler.toml` 的 `addresses` 里加上这个域，`wrangler deploy`
3. **等规则生效再让信进来**：新建的路由规则约 30–60 秒后才生效，此前对该域的地址回 `550 5.1.1`，
   发件方会当作永久失败退信。要迁移一个正在收信的域，先建好规则、发一封测试信确认收到，再切 MX

一笔投递有多个收件人、或者 Cloudflare 重试 Worker 时，同一封信可能被写进 R2 不止一次。
yuuka 入库时按内容去重，这是常规情况，不是故障。

## Email Sending：发信

yuuka 经 SMTP 把信交给一个出口（transport）。用 Cloudflare 发信时：

1. 在 Cloudflare 上为每个要发信的域 onboard Email Sending，按提示配 DNS（SPF、DKIM）。需要 Workers Paid
2. 在 dashboard 拿到 SMTP 凭据：主机 `smtp.mx.cloudflare.net`，端口 465（隐式 TLS），用户名和密码按控制台给的填
3. 在 yuuka 的配置里定义一个出口，`CAPS` 写 `cloudflare`（[配置参考](config.md#发信出口)），密码放进文件
4. 用 `yuuka identity add` 建发信地址并指向这个出口（[命令](cli.md#发信地址与授权)）

onboard 是域级的：一个域 onboard 之后，任何前缀都能以它发信。谁能用哪个地址发、能发到哪一步，
由 yuuka 的授权决定，不由 Cloudflare 决定（[为什么](../design/accounts.md#93-发信身份绑定传输这条不照做一定出事)）。

想留一条备用出口（别的 SMTP 服务商），它的 SPF `include` 和 DKIM 记录要**提前**配好：
等主出口出事再加 DNS，生效之前发出的信会被收件方判为伪造
（[细节](../design/drivers.md#104-抽象漏出的四件事)）。
