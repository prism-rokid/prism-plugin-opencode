# prism-plugin-opencode

[Prism](../prism-hub) PluginBridge 插件：把 [opencode](https://opencode.ai) 的本地 server 接入 Prism，让手机/眼镜等远端客户端可以列出、续聊、驱动 opencode 会话。

- **集成模式**：`protocol-native`（直连 opencode server 的 HTTP + SSE 协议，不做桌面自动化）
- **插件协议**：PluginBridge `v4`（`PRISM_PLUGIN_PROTOCOL_VERSION=4`）
- **传输**：Hub 以子进程方式拉起适配器，`stdio` 上跑换行分隔的 JSON-RPC
- **OpenCode 兼容性**：自动识别 V1/V2；真实服务验证版本为 `1.19.4` 和 `2.2.0`。迁移方案与验证范围见 [V2 适配说明](docs/V2-MIGRATION.md)。

## 架构

```
Prism Hub (TUI / 桌面 / 远程客户端)
   │  spawn + stdio 行协议（PluginBridge v4，snake_case JSON）
   ▼
prism-plugin-opencode 适配器  (cmd/opencode-adapter → ./bin/prism-plugin-opencode)
   │  internal/adapter  →  internal/opencode (唯一使用 net/http 的包)
   │  HTTP (JSON) + SSE (V1 /event，V2 /api/event)
   ▼
opencode server   （`opencode serve --port 4096`，或 TUI/桌面内嵌的随机端口实例；
                    `OPENCODE_PASSWORD` / `OPENCODE_SERVER_PASSWORD` 启用 basic auth，用户名 opencode）
```

- TUI 与桌面共用同一条链路：Hub 不感知 opencode，只看到 PluginBridge 会话。
- server 不在线时，Hub 可按 manifest 的 `managed_launch_specs` 拉起
  `opencode serve --port 4096`（`ready: probe`）。
- 详细分工与 API 事实见 [docs/CONTRACT.md](docs/CONTRACT.md)。

Windows 目录兜底：服务地址可以用 `PRISM_OPENCODE_SERVER_URL` 明确指定；
自动发现时可用 `PRISM_OPENCODE_STATE_DIR` 指向含 `service.json` 的目录，
用 `PRISM_OPENCODE_DATA_DIR` 指向实例文件目录。目录不可用时仍尝试本机 4096 端口。

## 仓库布局

| 路径 | 说明 |
|---|---|
| `pluginbridge-plugin.yaml` | 插件清单（须过 `../prism-plugin-sdk/conformance/manifest.schema.json`） |
| `cmd/opencode-adapter/` | 适配器入口（`--server-url`，默认 `auto` 走发现） |
| `internal/opencode/` | opencode HTTP+SSE 客户端（唯一允许 `net/http` 的包） |
| `internal/adapter/` | PluginBridge 适配器（协议版本 4） |
| `conformance/fakeserver/` | 可独立构建的假 opencode server（全部契约端点 + 可脚本化 SSE） |
| `conformance/driver/` | stdio 协议端到端驱动（probe→…→waitForRun 断言） |
| `conformance/validate_manifest/` | manifest 严格校验器（通用 JSON-Schema 子集解释器，stdlib 实现） |
| `conformance/run.sh` | 一键端到端：校验 manifest → 构建 → 起 fakeserver → 跑 driver |

## 开发

```bash
go build ./...          # 全部包
go test ./...           # 单元测试（httptest / mock，不打真实网络）
go vet ./...
go build -o bin/prism-plugin-opencode ./cmd/opencode-adapter   # 产出插件二进制（manifest.command 指向它）
```

manifest 校验（严格模式；`managed_launch_specs` 为 Hub 定义字段，见下）：

```bash
go run ./conformance/validate_manifest \
  -schema ../prism-plugin-sdk/conformance/manifest.schema.json \
  -manifest <(ruby -ryaml -rjson -e 'print JSON.generate(YAML.safe_load(File.read(ARGV[0])))' pluginbridge-plugin.yaml)
```

## Conformance（端到端）

```bash
./conformance/run.sh
```

脚本会：

1. 用 SDK 的 valid/invalid fixture 先自检校验器，再校验本插件 manifest；
2. 构建 `bin/prism-plugin-opencode`、fakeserver 与 driver（后两者进临时目录）；
3. 在随机空闲端口拉起 fakeserver（stdout 首行 `LISTEN <url>`）；
4. driver 启动适配器（注入 `PRISM_PLUGIN_PROTOCOL_VERSION=4` 与 `--server-url` /
   `OPENCODE_SERVER_URL`），依次断言 `probe → discover → listSessions → subscribe →
   send → waitForRun →（plugin.event）→ close`，PASS 退出码 0。waitForRun 先于事件断言
   执行，保证由真实终态事件（`session.status type=idle`）而非超时兜底命中。

手工把玩 fakeserver（形态与真机 1.19.4 抓包一致）：

```bash
go run ./conformance/fakeserver -port 0 -prompt-delay-ms 400   # 起服务
curl -N localhost:<port>/event &                                # 订阅 SSE（首个事件 server.connected）
curl -X POST -d '{"parts":[{"type":"text","text":"hi"}]}' localhost:<port>/session/ses_conformance_001/prompt_async -o /dev/null -w '%{http_code}\n'
# SSE 依次出现 session.status(busy) → message.updated(用户) → message.updated(assistant 部分)
#   → message.part.updated → message.updated(assistant 终稿) → session.status(idle)
```

可选：`CONFORMANCE_SERVER_PASSWORD=xxx ./conformance/run.sh` 让 fakeserver 要求
basic auth 并把密码经 driver 传给适配器，覆盖 `OPENCODE_SERVER_PASSWORD` 链路。

driver 对 `waitForRun` 的 runID 约定：优先取插件事件 payload 里的 `run_id`，
其次 send 回执的 `native_message_id`，最后空串（"当前 run"）。适配器实现时三选一
支持即可，driver 会按序尝试并在输出里报告命中了哪种。

## 真实 CLI 冒烟

安装本插件目标版本的 CLI（[OpenCode 官方安装文档](https://opencode.ai/docs/)）：

```bash
npm install -g opencode-ai@1.19.4 --registry=https://registry.npmmirror.com
opencode --version
```

`conformance/real_smoke.py` 用真实 PluginBridge stdio 连接真实 OpenCode v1 server，
覆盖鉴权、发现、挂载、会话列表、详情、默认模型配置、新消息、SSE 文本及消息关联、
waitForRun、可见性、实时/快照历史、运行中中断、新建会话和关闭。

Panel 的新建入口使用草稿协议：先 `openDraft`，可 `controlDraft(model.switch)`，
首条消息调用 `startDraftWithMessage` 才创建会话。草稿模型只作用于该次新建；
已有会话的模型切换使用 OpenCode 原生会话模型接口，不修改其他会话或项目默认配置。

先准备**独立配置/数据目录与临时项目**并启动 server，配置可用模型后运行：

```bash
go build -o bin/prism-plugin-opencode ./cmd/opencode-adapter
# OPENCODE_SERVER_PASSWORD 应与临时 server 保持一致；无密码时不设置。
python3 conformance/real_smoke.py \
  --server-url http://127.0.0.1:14096 \
  --cwd /tmp/prism-opencode-project \
  --model <providerID/modelID> \
  --output-dir /tmp/prism-opencode-results
```

脚本会创建会话、调用真实模型并消耗 token；模型控制会修改测试会话的原生模型。
输出目录保存适配器日志和协议帧，不保存密码。不要指向日常会话或项目。
本次验证与问题修复见 [docs/SMOKE-2026-10-26.md](docs/SMOKE-2026-10-26.md)。

## 发布

本插件遵循 SDK 的 Prism Plugin Registry 流程，与 `prism-plugin-openclaw` 一致：
打 `v<semver>` tag 后，GitHub Actions 在四个平台构建、测试、协议探针并打包；
随后生成含 SHA-256 的 `plugin-release.json`，创建不可变 GitHub Release。Registry 收到
`release.published` 后进行审核，审核通过的 stable Release 才会出现在 Hub/Panel 目录中。

发布仓库须先由 Prism 维护者注册并安装 Registry GitHub App。不要上传 OSS、不要覆盖既有
tag 或 Release 资产，也不要在仓库或 Actions 中配置 OSS、Registry webhook 或管理员凭据。
发布前确保 `pluginbridge-plugin.yaml`、`package.json` 和 tag 的版本完全一致。

## 与 openclaw / codex 插件的差异

| | opencode（本仓库） | openclaw | codex |
|---|---|---|---|
| 集成模式 | `protocol-native`（HTTP+SSE server） | `protocol-native`（gateway 协议） | `desktop-automation`（Node，CDP/桌面） |
| 原生实现 | Go 单二进制 | Go 单二进制 | TypeScript（`${runtime.node}`） |
| 事件订阅 | 每会话 `subscribe`（SSE 总线） | plugin-wide watch + approval | 桌面前台 watcher |
| 能力面 | 核心集：list/read-history/interrupt/wait-run/control/attach/start-with-message/双向同步（见 manifest） | 全家桶（approval、plugin-wide watch、read status 等） | 仅 `native_visible_input` + forward sync |
| server 发现 | `--server-url` / V2 service.json / 4096 / V1 实例文件 | `OPENCLAW_GATEWAY_URL` / auto | 桌面进程 |

## Manifest 备注

`managed_launch_specs` 是 Prism Hub 定义的托管启动声明（`prism-hub docs/13 §2.1`、
`internal/plugin/manifest.go`：`kind: service` + `start` argv + `ready: probe`），
`../prism-plugin-sdk/conformance/manifest.schema.json` 尚未收录该字段（schema 落后于
Hub；openclaw/hermes/codex 的清单同样携带它）。`conformance/run.sh` 因此在校验时以
`-allow-extra managed_launch_specs` 显式容忍这一项并打印 WARN，其余字段仍然严格校验；
SDK schema 更新后应移除该豁免。
