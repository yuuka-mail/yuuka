# 参与开发

这里写开发的具体做法：工具链、本地环境、检查和提交格式。
项目的规矩（哪些决定不可逆、品味往哪边偏、怎样算做完）写在 [`AGENTS.md`](AGENTS.md)，
人和 agent 遵守同一套，动手前请先读。系统为什么是现在这个样子，见 [`docs/design/`](docs/design/README.md)。

## 工具链

版本由 [`mise.toml`](mise.toml) 固定，任务也定义在那里：

```bash
mise install        # 首次：装好固定版本的 Go、golangci-lint、Node、pnpm
mise run check      # lint（含 vet 与格式检查）+ go test -race + WebUI 与 Worker 检查
mise run fmt        # 就地格式化：Go 用 golangci-lint，web/ 与 worker/ 用 oxfmt
mise run db:down    # 用完后删掉测试用的 Postgres 容器（test 会自动拉起）
```

`mise run check` 和 CI 跑的是同一条命令，通过了才算做完。

- **Node 的版本只写在 [`.node-version`](.node-version)**（Node 生态通用的文件，fnm、编辑器、CI 都认），
  mise 也从它读；其余工具的版本写在 `mise.toml`
- **JS 一律用 pnpm**，`web/` 与 `worker/` 是同一个 workspace、一份 `pnpm-lock.yaml`。
  pnpm 的版本写在根 `package.json` 的 `packageManager`，与 `mise.toml` 不一致时 pnpm 会直接报错。
  [`pnpm-workspace.yaml`](pnpm-workspace.yaml) 强制两条规则：**发布不满 14 天的版本装不上**，
  **安装脚本只对 `allowBuilds` 里审过的包放行**——新增需要安装脚本的依赖时，在那里显式加一行并说明理由
- 不要绕过 mise 直接用系统里别的版本；需要新工具时加进 `mise.toml` 并固定版本。
  只被 `go generate` 调用的 Go 工具（sqlc 等）用 `go.mod` 的 `tool` 指令固定
- lint 规则在 [`.golangci.yml`](.golangci.yml)。`//nolint` 必须指明 linter 并写理由
- `check` 也检查格式，没格式化的代码过不了；提交前跑一次 `mise run fmt`。
  前端的 lint 规则在 [`web/.oxlintrc.json`](web/.oxlintrc.json)，格式化配置在各自目录的 `.oxfmtrc.json`。
  整体重新格式化的提交登记在 [`.git-blame-ignore-revs`](.git-blame-ignore-revs)，
  本地 `git config blame.ignoreRevsFile .git-blame-ignore-revs` 后 blame 会跳过它们
- 新的检查（Worker 测试、前端检查等）随对应代码一起加进 `check`，不提前加

## 本地开发环境

开发库 `yuuka_dev` 和 `local/dev/` 都不进 git；R2 配置读 `mise.local.toml`。

```bash
mise watch dev:serve --restart   # 后端 127.0.0.1:31480，改 Go 代码自动重编译重启
mise run dev:web                 # 前端 http://localhost:31473，Vite 热更新，/api 与 /auth 转给后端
mise run dev:enroll              # 首次：打印一次性 passkey 注册链接
mise run dev:reset               # 清空开发库和原件，重新灌合成邮件
```

passkey 绑定浏览器看到的源，所以开发时一律从 `http://localhost:31473` 进，不要直接开 31480。
截图、调试 UI 用这套环境，不要另起端口手拼环境变量。

- **非标准端口一律在 31400–31499 之间**：测试 Postgres 是 `127.0.0.1:31432`，
  其他需要固定端口的东西（开发服务、本地依赖）也在这个段里挑，不占别的项目常用的端口
- 本机配置和凭据（如 Cloudflare token）放 gitignore 的 `mise.local.toml`，
  或仓库外的文件由它加载，不进被跟踪的文件

## 提交

遵循 [Conventional Commits 1.0](https://www.conventionalcommits.org/zh-hans/v1.0.0/)：

```
<type>(<scope>): <描述>

<正文：为什么、取舍>

<footer>
```

- **type**：`feat` `fix` `docs` `refactor` `perf` `test` `build` `ci` `chore` `revert`
- **scope** 可选，取改动所在的区域，和目录对应：`design`（`docs/design/`）、`operate`（`docs/operate/`）、
  `integrate`（`docs/integrate/`）、`deploy`，以及
  `ingest` `store` `outbox` `transport` `api` `mcp` `worker` `web` 等 `internal/` 包名或顶层目录。
  跨多个区域就省略 scope
- **提交信息全部用英文**（文档仍是中文）。描述用祈使句、小写开头、不加句号，
  标题行不超过 72 字符，如 `feat(ingest): reconcile pending markers against raw objects`
- **正文**写为什么和取舍，不复述 diff，按 72 字符折行
- **破坏性变更**用 `!` 并加 `BREAKING CHANGE:` footer。触及 [`AGENTS.md`](AGENTS.md)「单向门」的改动
  （Worker 契约、对外标识、迁移语义）一律按破坏性变更标注
- 一个提交一件事。agent 不主动 push，除非被要求
