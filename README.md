# fnOS LLM Proxy Suite

通过 fnOS / FN Connect 统一网关安全访问自建 LLM API 的一套代理方案，包含两个 Go 项目：

```text
Codex / Claude Code / OpenAI 兼容客户端
                  │
                  ▼
       fnconnect-proxy（本地）
       127.0.0.1:13431
                  │
                  │ FN Connect 会话 Cookie
                  ▼
https://fnos.net/<FN_ID>/app/llmproxy
                  │
                  ▼
          fnOS 统一网关
                  │
                  ▼
 /var/apps/llmproxy/target/app.sock
                  │
                  ▼
        fnos-llmproxy（NAS）
                  │
                  ▼
         可配置的 LLM 上游
```

## 项目组成

### `fnconnect-proxy/` — v1.0.7

运行在客户端主机，或与 Codex 位于同一个容器中。

主要功能：

- 登录 fnOS，并以 `0600` 权限在本地保存会话凭据。
- 自动附加 FN Connect / 统一网关所需 Cookie，无需把这些凭据写入 Codex 配置。
- 原样保留 LLM 请求中的 `Authorization` 请求头。
- SSE 流式响应即时转发，不缓存 token stream。
- 支持 `/app/<应用>` 与 `/<FN_ID>/app/<应用>` 两种统一网关路径。
- 后台每 30 分钟检查一次 token 状态，但只有 token 进入刷新窗口时才建立控制连接；不会再每 30 分钟强制刷新。
- 当前 `fnos-token` 失效时可使用 `longToken` 续期，并读取运行期间重新登录写入的新凭据。
- CLI 内置 `start` / `stop` / `restart` / `status` / `logs`，直接管理后台进程。

详细说明见 [`fnconnect-proxy/README.md`](fnconnect-proxy/README.md)。

### `fnos-llmproxy/` — v1.2.1

以 fnOS FPK 应用形式运行在 NAS 上。

主要功能：

- 提供 `:13430` TCP 入口，供局域网直接访问。
- 提供 `/var/apps/llmproxy/target/app.sock` Unix Socket，接入 fnOS micro-app 统一网关。
- 统一网关路径为 `/app/llmproxy`。
- 支持可配置的 HTTP(S) 上游。
- 支持 SSE 与 WebSocket 反向代理。
- 在向真实 LLM 上游转发前移除 fnOS 会话 Cookie，避免认证凭据泄漏。

详细说明见 [`fnos-llmproxy/README.md`](fnos-llmproxy/README.md)。

## 快速开始

先安装或构建 fnOS 端应用并配置上游 URL，然后在客户端构建本地代理：

```bash
cd fnconnect-proxy
go build -o fnconnect-proxy ./cmd/fnconnect-proxy
```

首次登录：

```bash
./fnconnect-proxy login \
  --server <FN_ID>.fnos.net \
  --target https://fnos.net/<FN_ID>/app/llmproxy \
  --username <FNOS_USER>
```

这里 `server` 用于 fnOS 登录/刷新控制流；`target` 用于实际 LLM HTTP 流量。将 FN ID 放在 HTTPS 路径中，可以避免它直接出现在 LLM 数据流的目标主机名里。

旧形式仍兼容：

```text
https://<FN_ID>.fnos.net/app/llmproxy
```

后台启动和管理代理：

```bash
./fnconnect-proxy start
./fnconnect-proxy status
./fnconnect-proxy restart
./fnconnect-proxy stop
```

测试 LLM API：

```bash
curl http://127.0.0.1:13431/v1/models \
  -H 'Authorization: Bearer YOUR_LLM_API_KEY'
```

## 请求链路

推荐的统一网关模式下，请求路径如下：

```text
客户端请求 /v1/responses
        ↓
fnconnect-proxy
        ↓ 自动附加 fnOS 会话 Cookie
https://fnos.net/<FN_ID>/app/llmproxy/v1/responses
        ↓
fnOS 统一网关
        ↓
app.sock
        ↓ 自动剥离 /app/llmproxy 前缀
fnos-llmproxy
        ↓ 删除 fnOS 会话 Cookie
配置的 LLM 上游 /v1/responses
```

## Token 刷新策略

`fnconnect-proxy` 每 30 分钟只做一次本地到期检查。当前默认刷新窗口为：

- `entry-token`：到期前 2 小时进入刷新窗口。
- `fnos-token`：到期前 30 分钟进入刷新窗口。

因此在 token 仍充足有效时，不会为了定时检查而建立 fnOS WebSocket 控制连接。若登录时按 8 小时记录短期 token，有效状态下通常会在接近刷新窗口时才产生控制流，而不是固定每 30 分钟访问 `<FN_ID>.fnos.net`。

网关明确返回认证失效时，代理仍可能立即执行恢复流程，不受上述定时检查周期限制。

## 构建与测试

运行两个项目的单元测试和静态检查：

```bash
(cd fnconnect-proxy && go test ./... && go vet ./...)
(cd fnos-llmproxy && go test ./... && go vet ./...)
```

构建 fnOS FPK：

```bash
cd fnos-llmproxy
./build.sh
```

生成的二进制文件和 FPK 包默认被 `.gitignore` 排除，不进入 Git 历史。

## Codex 配置示例

`~/.codex/config.toml`：

```toml
model = "YOUR_MODEL"
model_provider = "fnos"

[model_providers.fnos]
name = "LLM via FN Connect"
base_url = "http://127.0.0.1:13431/v1"
env_key = "LLM_API_KEY"
wire_api = "responses"
requires_openai_auth = false
```

然后设置上游 API Key：

```bash
export LLM_API_KEY='YOUR_API_KEY'
codex
```

## 安全说明

- 不要提交 `~/.config/fnconnect-proxy/config.json`。
- `longToken`、`fnos-token`、`entry-token` 都应视为敏感登录凭据。
- 默认只让 `fnconnect-proxy` 监听 `127.0.0.1`；除非明确了解风险，否则不要改成 `0.0.0.0`。
- `fnos-llmproxy` 在请求离开 NAS 前会移除 fnOS 认证 Cookie。
- 不要把真实 FN ID、账号、API Key 或登录 Cookie 提交到仓库。

## 当前版本

- `fnconnect-proxy`: **v1.0.7**
- `fnos-llmproxy`: **v1.2.1**

## 许可证

MIT，见 [`LICENSE`](LICENSE)。

`fnconnect-proxy` 的协议行为参考公开的 fnOS 社区实现独立完成，相关说明见 [`fnconnect-proxy/NOTICE`](fnconnect-proxy/NOTICE)。
