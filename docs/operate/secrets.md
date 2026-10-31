# 第三方机密

yuuka 拿去向第三方证明身份的凭据（现在是出口的 SMTP 密码和 Telegram 的 bot token）存在库里，用一把主机上的钥匙（KEK）加密。
库和它的备份里只有密文；KEK 留在主机上，不进库、不进备份卷、不写进环境变量的值
（[为什么](../design/secrets.md#171-进库的和留在主机的)）。

没有 KEK 时其余一切照常：收信、WebUI、REST、MCP、提交口都不受影响，只是需要密码的出口上锁，信留在队列里等。
KEK 丢了要把每个第三方密码重新输一遍，但不丢信。

## 生成 KEK

```bash
openssl rand -base64 32
```

输出是一行 base64，就是 KEK 文件的全部内容。**另外离线保存一份**（密码管理器，或者抄在纸上）：
它不在 `pg_dump` 里，也不该和 dump 放在一起。和库的备份一起泄露时，加密就没有意义了。

## 挂载

KEK 是一个文件，`YUUKA_KEK_FILE` 写它在容器里的路径。用 Docker Compose 时作为 Compose secret 挂进去，写在
`compose.override.yaml`（和 `compose.yaml` 放一起，Compose 会自动叠加）：

```yaml
services:
  yuuka:
    secrets: [yuuka-kek]
secrets:
  yuuka-kek:
    file: ./secrets/yuuka-kek   # secrets/ 和这个文件都在 .gitignore 里
```

```bash
install -d -m 700 secrets
(umask 077; openssl rand -base64 32 > secrets/yuuka-kek)
chown 65532:65532 secrets/yuuka-kek && chmod 400 secrets/yuuka-kek   # 镜像以 nonroot（65532）运行
```

`yuuka.env` 里写 `YUUKA_KEK_FILE=/run/secrets/yuuka-kek`。

- 文件给 `0400`、属主是容器里的用户：yuuka 只读它，不写它
- **不放在备份卷上**，把 `deploy/` 复制出机器做异地备份时也别带上 `secrets/yuuka-kek`；KEK 用上面的离线副本备份
- `docker compose run --rm yuuka transport …` 复用同一个服务定义，所以管理命令也读得到它

不用 Compose 时同理：一个只有运行 yuuka 的用户能读的文件，不在数据库备份所在的目录里。

`serve` 启动时读 KEK：没设 `YUUKA_KEK_FILE` 时照常启动，需要密码的出口上锁；设了但文件读不了或格式不对，拒绝启动。
有出口上锁时，`serve` 启动时和之后每 10 分钟报一次 ERROR `transports locked`，只能走它们的信在 `yuuka outbox ls` 里
是 `queued`，错误写着原因（[排障](troubleshooting.md#信发不出去)）；出口链上有不需要密码、或密码解得开的备用时，信走备用。

## 从环境变量升级

以前的版本把出口写在环境变量里（`YUUKA_TRANSPORTS`、`YUUKA_TRANSPORT_<NAME>_*`）。这些变量**已弃用**，
只在这一个版本里还有效，下一个版本删掉。升级就是让 `serve` 把它们导入库里一次，然后删掉：

1. 先备份：`docker compose exec backup sh /usr/local/bin/yuuka-backup once`
2. 按上文生成并挂载 KEK，在 `yuuka.env` 里设 `YUUKA_KEK_FILE`。旧的出口变量和密码文件的挂载先**留着**：导入要读它们
3. 换新镜像并启动：`docker compose up -d`。`serve` 启动时对每个环境变量里的出口：
   - 库里还没有它，或者只有名字的占位：写进库里（主机、端口、用户名、caps，密码从 `_PASSWORD_FILE` 读出后加密），
     日志里有 `imported transport from the environment into the database`，审计里记一条 `config.import`（`via: env`）
   - 库里已经配置好了：以库为准，环境变量被忽略，日志里报 WARN `transport is configured in the database; the environment's is ignored`
   - 库里被 `transport rm` 过：保持停用，同样报 WARN

   几个 `serve` 同时启动也只导入一次；之后每次启动都不会再写
4. 核实：

   ```bash
   docker compose run --rm yuuka transport ls       # STATE 是 configured，SECRET 是 set
   docker compose run --rm yuuka secret check       # 每一行都是 ok
   docker compose run --rm yuuka transport test cloudflare   # 能登录，不发信
   ```

5. 从 `yuuka.env` 删掉 `YUUKA_TRANSPORTS` 和全部 `YUUKA_TRANSPORT_<NAME>_*`，从 `compose.override.yaml` 删掉密码文件的挂载，
   删掉 `secrets/` 里的密码文件，`docker compose up -d`。不删的话，每次启动都会报上面那条 WARN

**没设 KEK 就升级**也不会停止发信：环境变量里的出口照旧从内存里工作，启动时报一条 WARN 说没有导入。
但下一个版本不再读这些变量，在那之前要完成上面的步骤。

之后改出口一律用 [`yuuka transport`](cli.md#出口与第三方机密)：换密码是 `transport set <名字> --password-file -`，
下一封信就用新的，不用重启。

## Telegram 从环境变量升级

Telegram 以前也是环境变量：`YUUKA_TELEGRAM_BOT_TOKEN`、`YUUKA_TELEGRAM_CHAT_ID`、`YUUKA_TELEGRAM_OPERATOR_ID`、
`YUUKA_TELEGRAM_OPERATOR`。它们和出口的变量一样**已弃用**，下一个版本删掉。现在渠道、绑定和订阅在库里，
bot token 用 KEK 加密（[`yuuka channel`](cli.md#通知渠道)）。步骤和上面的出口一起做：

1. 挂好 KEK、设 `YUUKA_KEK_FILE`，四个 Telegram 变量先**留着**，`docker compose up -d`
2. `serve` 启动时把它们导入一次：一个叫 `telegram` 的渠道（先用 `getMe` 问 Telegram 拿 bot 的用户名，连不上 Telegram 时只缺用户名，照样导入），
   一条绑定：chat 是 `YUUKA_TELEGRAM_CHAT_ID`，属于 `YUUKA_TELEGRAM_OPERATOR` 指定的人（没设时是库里唯一的那个人），
   设了 `YUUKA_TELEGRAM_OPERATOR_ID` 时这个用户的撤回按钮算数，订阅全部四类事件，也就是以前那个 chat 收到的。
   日志里有 `imported YUUKA_TELEGRAM_* as channel telegram`，审计记一条 `config.import`（`via: env`）。
   这是唯一不经过 `/start` 确认的绑定：环境变量本身就是主机上的确认。之后每次启动都不再导入，几个 `serve` 同时启动也只导入一次
   - 导入不了时（库里有好几个人又没设 `YUUKA_TELEGRAM_OPERATOR`，或者还一个人都没有）日志会说为什么，变量照旧工作；
     先 `admin enroll`，或设上 `YUUKA_TELEGRAM_OPERATOR`，再重启一次
   - 同一个 bot 已经是库里的一个渠道（比如用 `channel add` 加过），不导入，直接用库里的
3. 核实：

   ```bash
   docker compose run --rm yuuka channel ls          # 渠道 telegram，一条绑定，四个事件
   docker compose run --rm yuuka secret check        # bot token 那一行是 ok
   docker compose run --rm yuuka channel test telegram
   ```

4. 从 `yuuka.env` 删掉四个 `YUUKA_TELEGRAM_*`，`docker compose up -d`。不删的话每次启动报 WARN，说它们被忽略

**没设 KEK** 时这四个变量照旧工作，和以前完全一样，启动时报一条 WARN。库里的渠道这时都上锁，不轮询也不发送，
每 10 分钟报一次 ERROR。

以后加人、加 chat 都用 `channel bind`，不再手敲 chat id 和用户 id。

## 换 KEK

KEK 怀疑泄露、或者按周期轮换时。换的只是包装每条机密的钥匙，密文本身不动：

1. 生成新的 KEK，按上文挂进去（比如 `secrets/yuuka-kek-2`）；旧的继续挂着
2. `YUUKA_KEK_FILE` 指向新的，`YUUKA_KEK_PREVIOUS_FILES` 写旧的（逗号分隔，可以多个），`docker compose up -d`。
   这之后新写入的机密用新 KEK，旧的还能用旧 KEK 解开
3. `docker compose run --rm yuuka secret rewrap`：把每条机密改用新 KEK 包装，输出 `rewrapped N, already current M, failed 0`。
   有 `failed` 时按提示处理后再跑一次；重复跑是安全的，已经换好的会跳过
4. `docker compose run --rm yuuka secret ls`：KEY 列都是新的钥匙引用。再 `secret check` 确认都是 `ok`
5. 删掉 `YUUKA_KEK_PREVIOUS_FILES` 和旧 KEK 的挂载，`docker compose up -d`，销毁旧 KEK 的所有副本

**KEK 和库的备份一起泄露时**，换 KEK 不够：对方已经能解出明文。要到各个服务商那里把密码本身换掉，
再 `transport set <名字> --password-file -` 写进来。

## KEK 丢了

旧的密文再也解不开（`secret check` 报 `locked`）。生成一个新的 KEK 挂上，然后对每个出口重新输入密码：

```bash
docker compose run --rm -T yuuka transport set cloudflare --password-file - < cloudflare-password
docker compose run --rm -T yuuka channel add telegram --token-file - < bot-token   # 同一个 bot：换 token，绑定都还在
```

信、投递记录、授权都不依赖 KEK，什么都不丢。
