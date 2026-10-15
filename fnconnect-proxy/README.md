# fnconnect-proxy v1.0.5

# fnconnect-proxy

本地 Go helper：为 Codex / Claude Code / OpenAI SDK 自动维护 FN Connect `entry-token`。

链路：

```text
Codex -> http://127.0.0.1:13431/v1/... 
      -> 自动附加认证 Cookie（应用子域用 entry-token；/app 统一网关模拟浏览器会话，使用 mode=relay + language=zh-CN + fnos-token + entry-token）
      -> https://<FN_ID>.fnos.net/app/llmproxy/v1/...
      -> NAS llmproxy
      -> 你的上游 API
```

## 特性

- 首次用户名/密码登录，`stay=true` 获取 `longToken`
- 密码不写入配置文件
- 保存 `longToken` / `fnos-token` / `entry-token` / `sign_key`，配置文件权限 0600
- entry-token 默认按 8 小时管理，提前 2 小时刷新
- fnos-token 可用时直接刷新 entry-token
- 自动刷新只使用当前有效 `fnos-token` 换取新的 `entry-token`；不会自动轮换 `fnos-token`
- FN Connect 返回“暂无权限访问该服务”或统一网关精确返回 `invalid token` 时，仅安全刷新 `entry-token` 并重试一次
- 原样保留 Codex 的 `Authorization: Bearer ...`
- SSE 流式响应每个 chunk 立即 flush
- 不把 FN 登录 Cookie 暴露给 Codex 配置

> 注意：8 小时 / 25 天是当前社区客户端采用的生命周期假设，不是飞牛公开 SLA。v1.0.5 为避免再次破坏已验证可用的 `/app` 会话，不会自动使用 longToken 执行 `user.tokenLogin`；fnos-token 到期后会提示重新 `login`。

## 1. 首次登录

```bash
fnconnect-proxy login \
  --server YOUR_FN_ID.fnos.net \
  --target https://llmproxy.YOUR_FN_ID.fnos.net \
  --username YOUR_FNOS_USER
```

程序会交互读取 fnOS 密码。成功后配置写入：

```text
~/.config/fnconnect-proxy/config.json
```

权限为 `0600`。

也可从 stdin 提供密码，便于密码管理器集成：

```bash
printf '%s\n' "$FNOS_PASSWORD" | fnconnect-proxy login \
  --server YOUR_FN_ID.fnos.net \
  --target https://llmproxy.YOUR_FN_ID.fnos.net \
  --username YOUR_FNOS_USER \
  --password-stdin
```

## 2. 启动

```bash
fnconnect-proxy serve
```

默认监听：

```text
127.0.0.1:13431
```

检查：

```bash
curl http://127.0.0.1:13431/__fnconnect_proxy/health
```

然后测试 LLM API：

```bash
curl http://127.0.0.1:13431/v1/models \
  -H 'Authorization: Bearer YOUR_LLM_API_KEY'
```

## 3. Codex

`~/.codex/config.toml` 示例：

```toml
model = "YOUR_MODEL"
model_provider = "aitrading"

[model_providers.aitrading]
name = "AI Trading via FN Connect"
base_url = "http://127.0.0.1:13431/v1"
env_key = "AITRADING_API_KEY"
wire_api = "responses"
requires_openai_auth = false
```

然后：

```bash
export AITRADING_API_KEY='YOUR_API_KEY'
codex
```

如果你的上游只支持 Chat Completions 而不是 Responses API，需要按上游能力调整 Codex provider。

## 4. 自动启动（systemd --user）

```bash
mkdir -p ~/.local/bin ~/.config/systemd/user
cp fnconnect-proxy ~/.local/bin/
cp systemd/fnconnect-proxy.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now fnconnect-proxy.service
journalctl --user -u fnconnect-proxy -f
```

## 5. 管理

```bash
fnconnect-proxy status
fnconnect-proxy refresh
fnconnect-proxy logout
```

`refresh` 强制刷新 entry-token；`logout` 仅清除本地 token，不修改 NAS 账号。

## 安全

`longToken` 是长期登录凭据，应视同密码级秘密。不要上传 `~/.config/fnconnect-proxy/config.json`，不要粘贴到公开日志。helper 只监听 `127.0.0.1`，不要改成 `0.0.0.0`，除非你明确知道风险。

## v1.0.5

- 基于 v1.0.3 的已验证转发行为：`Authorization` 原样穿透，不做私有 Header 改名。
- `/app/...` Cookie 保持已实测可用的形态：`mode=relay; language=zh-CN; fnos-token=...; entry-token=...`。
- 自动刷新只更新 `entry-token`，绝不通过 longToken 自动替换当前 `fnos-token`。
- `fnos-token` 到期、或使用当前 fnos-token 刷新 entry-token 后网关仍拒绝时，明确要求重新执行 `fnconnect-proxy login`。
- 识别统一网关的 `HTTP 200 text/plain: invalid token`，但处理方式仍然是“只刷新 entry-token 一次”，不会触发 longToken 会话轮换。
- longToken 继续安全保存供后续验证/未来版本使用，但 v1.0.5 运行时不会自动消费它。
