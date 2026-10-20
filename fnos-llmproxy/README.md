# fnOS LLM Proxy

用于在 fnOS 上把 FN Connect / 统一网关请求转发到可配置的 HTTP(S) LLM API 上游。

## v1.2.0

v1.2.0 新增 fnOS 统一网关入口，同时保留原来的 TCP 端口入口：

```text
局域网直连:
http://NAS_IP:13430/v1/...

fnOS / FN Connect 统一网关:
https://<FN_ID>.fnos.net/app/llmproxy/v1/...
```

统一网关内部链路：

```text
https://<FN_ID>.fnos.net/app/llmproxy/v1/responses
        ↓ fnOS gateway
/var/apps/llmproxy/target/app.sock
        ↓ llmproxy 自动剥离 /app/llmproxy
/v1/responses
        ↓
配置的 upstream URL
```

例如上游设置为：

```text
https://api.example.com
```

则：

```text
https://<FN_ID>.fnos.net/app/llmproxy/v1/models
```

最终转发到：

```text
https://api.example.com/v1/models
```

## 功能

- HTTP / HTTPS 上游。
- fnOS 应用设置中可配置上游 URL。
- 默认上游：`http://127.0.0.1:3430`。
- TCP 入口：`:13430`。
- fnOS micro-app Unix Socket：`/var/apps/llmproxy/target/app.sock`。
- 统一网关前缀：`/app/llmproxy`。
- OpenAI / Anthropic 兼容 SSE 流式响应，不缓存 token stream。
- Go `httputil.ReverseProxy` WebSocket Upgrade 支持。
- 保留客户端 `Authorization`。
- 转发到真实 API 上游前删除 `Cookie` 中的 `entry-token`，避免 FN Connect 凭证泄漏给公网 API。
- x86_64 / arm64。

## 配置上游

安装或升级后：

```text
应用中心 → LLM Proxy → 设置 → 上游 URL
```

填写：

```text
https://api.example.com
```

保存后重启 LLM Proxy。

上游 URL 可以带基础路径。例如：

```text
https://api.example.com/openai
```

统一网关请求：

```text
/app/llmproxy/v1/models
```

先去掉 fnOS 前缀，再与 upstream base path 拼接，最终为：

```text
https://api.example.com/openai/v1/models
```

## fnconnect-proxy 配置

使用 `fnconnect-proxy`，target 必须是 FN Connect 子域名 + `/app/llmproxy`：

```json
{
  "target_url": "https://<FN_ID>.fnos.net/app/llmproxy"
}
```

`https://fnos.net/<FN_ID>/app/llmproxy` 会被 v1.0.9+ 拒绝：门户对该路径返回 SPA，不会进入 NAS。

Codex 仍然保持：

```toml
base_url = "http://127.0.0.1:13431/v1"
```

因此：

```text
Codex /v1/responses
  ↓
fnconnect-proxy
  ↓ 自动添加 Cookie: entry-token=...
https://<FN_ID>.fnos.net/app/llmproxy/v1/responses
  ↓
fnOS gateway → app.sock
  ↓
llmproxy
  ↓ 删除 entry-token
https://api.example.com/v1/responses
```

## 验证

局域网入口：

```bash
curl -v http://NAS_IP:13430/v1/models \
  -H 'Authorization: Bearer YOUR_KEY'
```

统一网关（浏览器已登录 fnOS 时可直接测试）：

```text
https://<FN_ID>.fnos.net/app/llmproxy/v1/models
```

使用 `fnconnect-proxy`：

```bash
curl -v http://127.0.0.1:13431/v1/models \
  -H 'Authorization: Bearer YOUR_KEY'
```

## 持久配置

上游 URL：

```text
${TRIM_PKGETC}/upstream_url
```

启动优先级：

1. `UPSTREAM_URL`
2. `${TRIM_PKGETC}/upstream_url`
3. `http://127.0.0.1:3430`

## 日志

```text
${TRIM_PKGVAR}/llmproxy.log
${TRIM_PKGVAR}/config.log
```

正常启动日志应同时出现：

```text
TCP :13430 -> https://...
fnOS unified gateway socket /var/apps/llmproxy/target/app.sock prefix=/app/llmproxy
```

## v1.2.1 安全改进

在向配置的上游转发请求前，会移除统一网关会话 Cookie（`fnos-token`、`fnos-long-token`）以及 `entry-token`，避免 fnOS 登录凭据泄漏给公网 API。
