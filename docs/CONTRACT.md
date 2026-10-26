# prism-plugin-opencode 内部契约

并行开发的共同契约。签名以 `internal/opencode/client.go` 为准；
本文件描述各包职责与 opencode API 事实依据，冲突时以官方文档为准并更新本文件。

## 包与责任人

| 路径 | 责任 | 说明 |
|---|---|---|
| `internal/opencode/` | 子智能体 A | opencode HTTP+SSE 客户端，唯一允许 `net/http` 的包 |
| `internal/adapter/` + `cmd/opencode-adapter/` | 子智能体 B | PluginBridge 适配器（协议版本 4），不得 import `internal/opencode` 之外的实现细节；通过自有 `Client` 接口（方法集与 `opencode.Client` 完全一致）解耦 |
| `pluginbridge-plugin.yaml`, `README.md`, `conformance/` | 子智能体 C | manifest（过 `../prism-plugin-sdk/conformance/manifest.schema.json` 校验）、fake opencode server、端到端一致性脚本 |

## 集成规则

- `internal/adapter` 定义 `type Client interface { ... }`，方法集逐字对应 `opencode.Client` 的公开方法（含 `Discover` 不在其中——discovery 由 main 调 `opencode.Discover` 后 `opencode.New` 注入）。
- `cmd/opencode-adapter/main.go`：flag `--server-url`（env `PRISM_OPENCODE_SERVER_URL`，默认 `auto` 走 `opencode.Discover`；显式 URL 优先，发现失败时回退 `http://127.0.0.1:4096` 以便 server 后启动时 probe 可恢复在线）、env `OPENCODE_SERVER_PASSWORD`；调 `pluginbridge.CheckProtocolVersion()`，然后 `pluginbridge.StdioServer{Adapter, In: os.Stdin, Out: os.Stdout}.Run(ctx)`。参照 `../prism-plugin-openclaw/cmd/openclaw-adapter/main.go`。
- Fake server (`conformance/fakeserver`) 必须实现：`GET /global/health`（客户端发现探活用）、`GET /session`、`POST /session`、`GET /session/:id`、`PATCH /session/:id`、`DELETE /session/:id`、`GET /session/:id/message`、`POST /session/:id/prompt_async`、`POST /session/:id/abort`、`GET /config`、`PATCH /config`、`GET /config/providers`、`GET /agent`、`GET /event`（SSE）。**已按 A 的真机抓包校准**（prompt body 为 `{"parts":[...]}`、message 列表为 `{info,parts}` 信封、providers 为 `{providers:[...],default}` 对象、session 创建 directory 走 query、SSE 事件 properties 顶层带 `sessionID`、终态事件推 `session.status{status:{type:"idle"}}`、每 ~15s `server.heartbeat`）。

## 事件语义（集成裁定，2026-10-26 D 轮）

- 真机 1.19.4 的运行终态信号是 `session.status`（`properties:{sessionID, status:{type:"busy"|"idle"}}`），心跳是 `server.heartbeat` 事件帧；`session.idle{sessionID}` 是并存旧信号。
- 适配器对二者等价识别：`session.status(type=idle)` 与 `session.idle` 都映射为 `run.completed`/`run.failed` 终态；`session.status(type=busy)` 与 transport 事件（server.connected/server.heartbeat）不外发。history live tail 对二者都会触发重载（diff 后无变化不外发）。
- fakeserver 只推真实形态（`session.status`），旧形态由适配器单测覆盖，避免 conformance 出现双终态事件。

## opencode server API 事实（子智能体 A 已实证：opencode-ai 1.19.4 真机 + 其 /doc OpenAPI 3.1 spec，2026-10-26）

> 目标版本为 v1（npm `opencode-ai` latest=1.19.4）。v2（anomalyco/opencode，beta）有破坏性差异，见本节末尾。

- 默认 `127.0.0.1:4096`；`OPENCODE_SERVER_PASSWORD` 启用 basic auth，用户名默认 `opencode`（错用户名/密码均 401，已实测）。
- JSON 字段为 camelCase（`parentID`、`modelID`、`providerID`）；session/message 时间在 `time.created`/`time.updated`，**毫秒 epoch**（实测对齐 `Date.now()`）。错误响应形如 `{"name":"NotFoundError","data":{"message":...}}`。
- Session 字段：`id/title/parentID/directory/time{created,updated}` + 未建模的 `slug/projectID/workspaceID/path/cost/tokens/share{url}/agent/model{id,providerID,variant}/version/metadata/permission/revert/summary`（后者保留在 `Session.Raw`）。
- **`POST /session` 的 `directory` 是 query 参数不是 body 字段**（body 仅 `parentID/title/agent/model/metadata/permission/workspaceID`）；带 `?directory=<路径>` 可把 session 绑到指定项目目录（实测）。`GET /session` 服务端已按 `time.updated` 降序返回。
- `GET /session/:id/message` 返回 **`[{info: Message, parts: Part[]}]` 信封**（不是裸 Message 数组）；顺序为时间正序。assistant 的 `modelID/providerID/cost` 在 info 顶层，user 的模型嵌在 `info.model{modelID,providerID,variant}`（客户端已归一化）。part 类型实测有 `text/tool/step-start/snapshot/reasoning/...`，text part 为 `{id,sessionID,messageID,type,text}`，非 text 字段全量保留在 `MessagePart.Raw`。
- `POST /session/:id/prompt_async`：body `{"parts":[{"type":"text","text":"..."}]}` 即可，返回 **204**（实测）；消息流靠 SSE 事件，不做同步 HTTP 轮询。`POST /session/:id/abort` 与 `DELETE /session/:id` 返回 boolean。
- **附件/图片**（1.19.4 OpenAPI + 用户桌面版持久化数据双重实证）：`parts` 可混入 `{"type":"file","mime":"image/png","filename":"shot.png","url":"data:image/png;base64,..."}`（`type/mime/url` 必填，`filename` 可选）；官方客户端发图即 data URL 内嵌形态（桌面版 opencode.db 的 part 表同款）。适配器把 hub 物化的本地附件读文件→base64 data URL→file part（≤20MB）；历史上附件只以 `metadata.attachments[{name,mime_type,kind}]` 元数据外显，绝不外泄 data URL。
- `GET /config/providers` 返回 `{providers:[{id,name,source,models:{modelID:{name,...}}}], default:{providerID:modelID}}`；`models` 是 map，ModelInfo 从 `providers[].id` + `models` 键 + `models[k].name` 归一化。`GET /agent` 会返回 `hidden:true` 的内部 agent（`title/summary/compaction`），客户端已过滤。
- 模型切换：现使用 `POST /api/session/:id/model` body `{"model":{"id":"modelID","providerID":"providerID"}}`，当前 1.19.4 `/doc` 中的 operationId 为 `v2.session.switchModel`，真机返回 204 并更新会话 `model`。详情优先读取会话原生模型，缺失时再取项目默认值。**旧实现的 `PATCH /config` 只更改项目默认模型，不会覆盖已有会话的模型；且会写项目配置、dispose/recreate 目录实例。** 插件不再以该操作实现会话模型切换。
- **`/event` 是目录级事件总线**，必须传 `?directory=<会话目录>`；省略时只订阅 server 启动目录实例，其他目录的会话虽可发消息并产生真实回复，但订阅和 waitForRun 收不到事件。Subscribe、WaitForRun 和 live history 均携带会话目录；`message.part.updated` 的 messageID 在 `properties.part.messageID`。
- **中断终态**：真机 abort 后的 idle 通常不带 `reason`，错误持久化在最新 assistant 的 `info.error`（`name: MessageAbortedError`）。终态投影结合最新 assistant 错误生成 `run.failed` + `reason: aborted`，不能把无 reason 的 idle 一律当成功，也不能回退显示上一次回复。
- 事件流 `GET /event` 为 SSE，帧格式 `data: {"id":"evt_...","type":"...","properties":{...}}\n\n`（单行 JSON）；首个事件必有 `server.connected`；实测事件：`session.created/updated{sessionID,info}`、`session.idle{sessionID}`、`session.status{sessionID,status:{type:"busy"|"idle"}}`、`message.updated{sessionID,info}`、`message.part.updated{sessionID,part}`、`session.error{sessionID,error{name,data}}`、`server.heartbeat{}`。每 ~15s 有 `server.heartbeat`（不是注释心跳）。
- 发现机制（实证）：**v1（含 1.19.4）不写任何实例/端口注册文件**——TUI 随机端口只打到 stdout，IDE 插件自己起 `opencode serve`；用户数据目录是 XDG 风格且**全平台统一** `$XDG_DATA_HOME/opencode`，默认 `~/.local/share/opencode`（macOS 亦然；`/Library/Application Support/opencode` 只是企业托管配置目录）。Discover 顺序：ExplicitURL → `http://127.0.0.1:4096` → 数据目录下宽容扫描 `instance/*.json`、`process/*.json`、`server.json`、`port.json`（识别 `{port}`/`{url}`，为将来版本兜底）；探活 `GET /global/health`（v1.x 有此端点，返回 `{"healthy":true,"version":...}`），回退 `GET /config`；401/403 视为存活。全部失败 `ErrServerOffline`。
- v2 差异备忘（beta，暂不适配）：`GET /session/:id/message` 变为 `{data:[...],cursor:{previous,next}}` 分页且默认倒序；SSE 事件 JSON 的 `properties` 改名为 `data`；心跳用 `: heartbeat` 注释行。

## 验证门槛

- Panel 新建会话必须实现 DraftOpener、DraftController、DraftStarter 并声明 CanOpenDraft；OpenDraft 仅保存内存草稿，ControlDraft 仅修改该草稿模型，StartDraftWithMessage 才携带所选模型创建原生会话并发送首条消息。草稿启动重试复用已创建的会话和回执，不重复创建或发送；过期、未知草稿、已启动后变更正文/控件必须失败。
- 原生 assistant `info.error` 必须投影为失败历史消息；无正文的 APIError 也显示可读错误，不能因空文本让 Panel 看起来一直没回复。

- Panel 打开已有会话通过 `ControlSession(action: conversation.select)`：headless server 没有桌面前台，插件必须 GET 目标原生会话并精确确认 ID 后返回 `ok/thread_id/details.selected`；不存在、已删除或 ID 不一致必须失败。该回执只确认身份，不补读 composer controls 或伪造完整 detail snapshot。

- A/B 各自包 `go build` + `go test` 绿（httptest / mock Client，不发真实网络请求）。
- C 的 manifest 通过 SDK schema 校验；fakeserver `go build` 通过。
- 子智能体 D 最终跑 `go build ./... && go vet ./... && go test ./...` 与 `conformance/run.sh`（stdio JSON-RPC 端到端），全绿才算完成。
