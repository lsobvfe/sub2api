# local-native

本机以两个独立进程运行：

```text
client :18081
  -> stream-hold-proxy (独立 Go module)
  -> official Sub2API :18082
```

Sub2API 的后端、前端、配置、Wire 和测试尽量保持 `upstream/main` 原样。Hold Proxy 的 API key 开关通过一个窄 hook 接入，运行时仍与 proxy 独立：

```text
local-native/extensions/stream-hold-proxy/
```

该模块不 import Sub2API，不读取其数据库、账号、分组或运行时设置。Sub2API 只在 API key 变更和启动时向本机 proxy 注册 key hash；proxy 的请求热路径只查本地注册表。每次重试都是一条新的普通 HTTP 请求，因此 API key 换分组、账号池变化和 Sub2API 重启都会在下一次请求中自然生效。

## 流式语义

代理仅接管 `STREAM_HOLD_PATHS` 中且 JSON body 为 `"stream": true` 的请求：

1. 完整复制客户端请求并发给 Sub2API。
2. 将该次 SSE 写入临时 spool，同时解析终止事件。
3. 只有看到 `response.completed`（或 `[DONE]`）才向客户端回放。
4. HTTP 错误、传输错误、`response.failed`、`response.incomplete`、无终止 EOF、流空闲和单次尝试超时全部丢弃并重试。
5. 总等待时间不限；客户端取消后立即停止。
6. 等待期间向客户端发送可解析但无业务输出的 SSE keepalive 事件，持续重置客户端 idle timeout。

这意味着失败尝试即使已经生成部分 token，也不会把半截内容或错误泄露给客户端。

## 运行文件

| 路径 | 说明 | Git |
|------|------|-----|
| `extensions/stream-hold-proxy/` | 独立代理和独立测试 | 跟踪 |
| `systemd/` | 两个系统服务的唯一模板 | 跟踪 |
| `scripts/update-and-restart.sh` | 同步、构建、部署和验收 | 跟踪 |
| `runtime/sub2api.env.example` | Sub2API 内部服务配置模板 | 跟踪 |
| `runtime/stream-hold-proxy.env.example` | 代理配置模板 | 跟踪 |
| `runtime/*.env` | 本机实际配置 | 忽略 |
| `runtime/work/` | 状态、spool、Sub2API 工作目录 | 忽略 |
| `build/` | 两个二进制 | 忽略 |

## 开关

本机控制页：

```text
http://localhost:18081/_stream-hold/
```

全局开关由代理自身持久化；API key 开关由 Sub2API 页面保存并同步到代理的 hash 注册表。停用后，新请求透明转发；已经被接管的请求继续完成，避免切换动作主动断开现有客户端。

## 更新

先提交工作树，再运行：

```bash
./local-native/scripts/update-and-restart.sh
```

VS Code task：`sub2api: update and restart`

脚本会：

1. 要求工作树干净，不再自动生成 snapshot commit。
2. fetch/merge `upstream/main`。
3. 使用固定的 pnpm 9 工具链非交互安装和构建前端。
4. 构建官方 Sub2API embed 二进制。
5. 构建独立代理并安装两个 systemd unit。
6. 重启内部 Sub2API；代理二进制未变化时不重启代理，因此已有客户端流会在 Sub2API 更新期间继续 hold。
7. 验证两个端口的唯一监听者、内部/外部 health 和前端 HTML。

代理自身测试：

```bash
cd local-native/extensions/stream-hold-proxy
go test ./...
```
