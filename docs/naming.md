# 命名、命令面与仓库布局

> yuuka 设计文档的一部分。节号沿用整份设计的全局编号，索引见 [docs/README.md](README.md)。

## 11. 命名与命令面

项目名 **yuuka**，取自项目吉祥物。吉祥物命名整个项目而不是其中某个组件，
所以不带 `-mail` 后缀（先例：Docker 的吉祥物 Moby 后来成了开源项目名）。
邮件只是第一个入口形态，§9 的设计迟早会接 IM / webhook / RSS，名字不该锁死在 mail。

下面是命令面的名字登记：子命令名是对外标识（AGENTS.md 的单向门），新增要先在这里登记。
已实现的子命令以 [`cmd/yuuka`](../cmd/yuuka/main.go) 为准，参数不在这里重复。

```
github.com/yuuka-mail/yuuka             # module
yuuka serve                             # 常驻：HTTP + ingest + 调度
yuuka migrate                           # 以 schema owner 身份应用待执行的迁移；部署里唯一持有 owner 凭据的一次性步骤（§4）
yuuka reconcile                         # 手动跑一轮 §2.1 的 reconcile，可选全量对账
yuuka ingest --once                     # 手动跑一轮 pull driver
yuuka reparse                           # 从不可变原件重建派生数据（§3.1、§4.2）
yuuka prefix ls                         # 列出前缀：状态、首次 / 末次投递、投递数
yuuka prefix claim                      # 认领前缀：其信不再降级，此后的新信推送（§4.3）
yuuka prefix unclaim                    # 取消认领，回到 discovered
yuuka prefix label|leaks                # 前缀注册表的标注与泄漏视图（§3.3）
yuuka token issue                       # 给 agent / service 签发 bearer token，只显示一次（§13.3）
yuuka token ls                          # 列出 token：主体、scope、各时间点，不含秘密
yuuka token revoke                      # 吊销 token，REST 与 MCP 下一次请求起失效
yuuka token issue --kind smtp           # SMTP 提交口的密码，用户名是凭据 id（§13.3、§13.6）
yuuka identity add|ls|rm                # 能发信的地址（精确或 *@zone）、出口、归属；rm 是停用（§9.3）
yuuka grant set|ls|rm                   # 谁能以哪个身份发信、发到哪一步（§13.5、§8.9）
yuuka outbox ls|retry|dead              # 出口状态机（§3.6）
yuuka account add|sync|caps             # 多账户（§9）
yuuka doctor                            # DNS / SPF / DKIM / 出口授权自检（§10.4.4）
yuuka admin enroll                      # 签发一次性 passkey 注册链接与离线恢复码（§13.7）
yuuka mcp --stdio                       # 本地 stdio 形态的 MCP（§8.7）；HTTP 形态内置在 serve
yuuka version                           # 打印版本
yuuka://message/<ulid>#part/2/para/7    # agent 锚点 scheme（§8.1）
```

### 11.2 名字不进邮件头

- `Message-ID: <ulid@example.com>`——域名部分用真实域名，本地部分是 ULID，不含项目名。
  只在允许自定 Message-ID 的出口上成立；Cloudflare 出口由平台生成，形如 `<36 位字母数字@From 的域>`，
  同样不含项目名（§10.4.1）
- `X-Mailer` 默认**不发**。Cloudflare 出口实测不加 `X-Mailer` / `User-Agent`，只加 `Feedback-ID` 和
  一条 `Received: from authenticated-submission.invalid`，都不带我们的名字或主机（§10.4.1）；
  其他出口接入时随「发给自己比对字节」一并实测，部分 provider 会自己加
- 自建 MX 那天 `Received` 会带 `yuuka/<version>`，那是加在入站信上给自己看的

### 11.3 撞名备注

`yuuka` 这个拼法顺带解决了可发现性问题：此前的顾虑是法国那个食品扫描 app
`Yuuka`（用户量很大），而双 u 的 `yuuka` 在软件领域没有同名项目，
`github.com/yuuka-mail/yuuka` 和 `yuuka` 这个命令名都是干净的。

module path 一旦有人 `go get` 过就不该再改，所以**趁现在还没推送，
org 名和项目名的拼法要一次对齐**（见 §11.4）。

### 11.4 仓库与 module path

```
github.com/yuuka-mail/yuuka            # module，org 段带后缀、主仓用裸名
                                       # （惯例同 golang/go、rust-lang/rust）
```

连字符只出现在 org 段，所以 module / package / binary 三处写法完全一致：
package 声明是 `yuuka`，binary 由 `cmd/yuuka` 决定。

**Cloudflare Worker 不单独开仓**，作 `worker/` 子目录。它和 Go 侧有三样东西
必须同步演进——HMAC 签名格式、R2 的 key 布局（`raw/YYYY/MM/<ulid>.eml`
与 `pending/<id>`）、ack 语义——分仓的版本漂移后果是**丢信**，正是 §3.5
最不能出的事。代价只是 CI 跑两套（`go test` + `wrangler deploy`）。

```
yuuka-mail/yuuka
├─ cmd/yuuka/      # CLI + server 入口（§11）
├─ internal/       # Go 包；怎么分包是双向门，以代码为准
├─ worker/         # Cloudflare Email Worker（TS）
├─ web/            # 前端，产物 embed 进二进制
├─ docs/           # 设计文档，按 § 拆分，索引见 docs/README.md
└─ testdata/       # .eml 语料（§10.5）与 Worker ↔ Go 契约向量（§2.2）
```

`ix64/spf` 直接 `require` 进来用于 §3.3 的辅助 SPF 判定（不参与 From 身份判断）；
org profile README 放 `yuuka-mail/.github/profile/README.md`。

---
