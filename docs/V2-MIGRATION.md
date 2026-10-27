# OpenCode V1 / V2 双协议适配

日期：2026-10-27。源码适配已完成，未创建新插件 Release、未替换正在运行的 Hub 插件。

## 计划与交付

1. 保留 V1 客户端契约，在 HTTP 边界识别协议并隔离 V2 的输入、分页、消息和事件差异。
2. 适配 V2 共享服务注册与密码；支持会话创建/读取/重命名/删除、模型切换、文字/图片发送、历史、实时文本和中断。
3. 对 V2 的 live-only 事件补充重连历史重同步，以及通过已接收用户消息 ID 后的持久化 idle 记录恢复错过的运行终态。
4. 运行 V1/V2 契约测试、竞态检测、协议冒烟，并连接两个真实版本的隔离服务验证。

以上步骤已执行。后续发布需要另行更新插件版本并走正常 Release / Registry 流程。

## 官方契约

- [V1 迁移指南](https://opencode.ai/v2/docs/migrate-v1)：server API 是有意的破坏性变更。
- [V2 OpenAPI](https://opencode.ai/v2/openapi.json)：本次以实际下载的契约和 `2.2.0` 服务交叉验证。
- [V2 客户端及共享服务](https://opencode.ai/v2/docs/build/client/)：共享服务注册位于 XDG state 目录，事件不重放。

本项目是 Prism 的外部 HTTP 集成，不加载 OpenCode 原生 JS 插件，因此迁移重点是服务接口和事件，不涉及原生插件 `setup()` 生命周期。

## 行为与边界

- `--server-url` / URL 环境变量优先；自动发现依次检查 V2 的 `$XDG_STATE_HOME/opencode/service.json`（默认 `~/.local/state/opencode/service.json`）、4096、旧实例文件。
- 只读取指向本机回环地址的服务注册文件；发现过程不启动、停止或迁移 OpenCode。
- `OPENCODE_PASSWORD` 优先于 `OPENCODE_SERVER_PASSWORD`；无显式密码时使用匹配的服务注册密码。
- 每个客户端通过 `/api/info` 识别 V2；404 或旧服务的 HTML 页面回退 V1。401、服务错误、未知版本不静默降级。服务大版本切换后需重新启动插件进程。
- V2 会话和消息列表遍历分页；重复游标返回错误，避免循环和静默截断。
- V2 `text/files` 转换为现有统一消息类型；图片发送使用 data URI，历史仅向 Prism 返回附件元数据。
- V2 事件使用 `data` 载荷，支持 Effect SSE 的 JSON 字符串编码；全局事件按原生目录及会话 ID 过滤，文本 delta 累积成现有 `message.part.updated`。
- V2 `execution.failed/interrupted` 返回失败终态，不能当成成功；已错过的终态仅匹配该客户端提交的用户消息之后的 idle 记录。
- V2 默认模型从 `/api/model/default` 读取；会话模型切换不修改全局配置。旧 `PatchConfig` 在 V2 明确返回不支持，避免误写新配置 API。
- 本次保留现有能力范围：新建草稿首条消息仍要求文本；已有会话支持图片。权限审批和问题表单未在此次新增，亦未宣称 V2 全量 API 兼容。

## 验证结果

- `go test -race ./...`、`go vet ./...`。
- `./conformance/run.sh`：V1 假服务完整 PluginBridge 链路，含图片附件。
- 真实 OpenCode `1.19.4`，独立 XDG 目录、端口 14098：`conformance/real_smoke.py` 通过新建草稿、模型切换、发送、文本 SSE、实时/快照历史、可见性和运行中中断。
- 真实 OpenCode `2.2.0`，独立 XDG 目录、端口 14096：`conformance/v2_smoke.py` 通过新建草稿、模型切换、发送、图片、文本 SSE、实时/快照历史、运行中中断和延迟调用 waitForRun 的终态恢复。
- 两个真实服务都接入本地 OpenAI-compatible 模拟模型（14097），不依赖云端凭据或推理。模型请求日志确认图片进入 `image_url` 内容。此结果验证真实 OpenCode 处理链路，不代表外部模型供应商或桌面 UI 已回归。
- 用户日常使用的 4096 服务及配置未修改。

## 重跑隔离 V2 冒烟

将 V2 二进制解包到临时目录，勿运行会覆盖全局 CLI 的安装器。所有 `XDG_CONFIG_HOME`、`XDG_DATA_HOME`、`XDG_STATE_HOME`、`XDG_CACHE_HOME` 指向独立临时目录；准备临时项目并在该配置目录的 `opencode/opencode.json` 写入：

```json
{
  "model": "prism-smoke/smoke",
  "providers": {
    "prism-smoke": {
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": { "baseURL": "http://127.0.0.1:14097/v1", "apiKey": "smoke", "provider": "prism-smoke" },
      "models": {
        "smoke": {
          "name": "Prism isolated smoke",
          "capabilities": { "tools": false, "input": ["text", "image"], "output": ["text"] },
          "limit": { "context": 16000, "output": 1000 }
        }
      }
    }
  }
}
```

在该隔离环境启动 `opencode serve --hostname 127.0.0.1 --port 14096`，再分别运行：

```sh
python3 conformance/mock_model.py --port 14097
go build -o bin/prism-plugin-opencode ./cmd/opencode-adapter
python3 conformance/v2_smoke.py \
  --server-url http://127.0.0.1:14096 \
  --cwd /absolute/path/to/disposable-project \
  --model prism-smoke/smoke
```

若服务设置密码，适配器进程的 `OPENCODE_PASSWORD` 必须一致。脚本会创建会话和测试 PNG；不要指向日常项目。验证结束后关闭测试服务及模拟模型。
