# fnconnect-proxy v1.0.7

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
- 每 30 分钟刷新一次 `entry-token`，不依赖固定的过期时间猜测
- 当前 `fnos-token` 失效或即将过期时，自动使用 `longToken` 换取新会话并更新本地凭据
- FN Connect 返回“暂无权限访问该服务”或统一网关精确返回 `invalid token` 时，先刷新 entry-token；仍被拒绝则使用 longToken 更新会话，最多重试两次
- `serve` 运行期间会一起更新重新登录写入的目标地址和凭据；并发刷新不会覆盖新登录或退出的配置
- 原样保留 Codex 的 `Authorization: Bearer ...`
- SSE 流式响应每个 chunk 立即 flush
- 不把 FN 登录 Cookie 暴露给 Codex 配置

> 注意：8 小时 / 25 天是本地状态显示使用的生命周期估计，不是飞牛公开 SLA。代理每 30 分钟主动刷新，并在旧会话被拒绝时尝试使用 `longToken` 续期；服务器仍拒绝时会在日志或请求错误中报告续期失败。

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

## 4. CLI 后台管理

编译后直接用 CLI 管理后台进程：

```bash
go build -o dist/fnconnect-proxy ./cmd/fnconnect-proxy
./dist/fnconnect-proxy start
./dist/fnconnect-proxy stop
./dist/fnconnect-proxy restart
./dist/fnconnect-proxy status
./dist/fnconnect-proxy logs
./dist/fnconnect-proxy logs --lines 200
```

`start` 在后台运行代理，终端退出后继续运行；已启动时返回现有实例的状态。`stop` 等待请求处理和连接关闭后退出。`restart` 默认保留运行中实例的监听地址，也可通过 `--listen` 修改。`status` 同时显示后台进程状态、登录配置和 token 有效期。`serve` 继续用于前台运行，CLI 启停仅管理 `start` 创建的后台实例。

```bash
./dist/fnconnect-proxy start --listen 127.0.0.1:13431
./dist/fnconnect-proxy restart --listen 127.0.0.1:13432
```

默认运行记录和日志保存在登录配置文件旁的 `run/` 目录（例如 `~/.config/fnconnect-proxy/run/`）。一个运行目录管理一个后台实例；可用以下环境变量指定独立配置和运行目录，后续管理命令使用相同变量：

```bash
export FNCONNECT_PROXY_CONFIG=/path/to/config.json
export FNCONNECT_PROXY_RUN_DIR=/path/to/runtime
./dist/fnconnect-proxy start
./dist/fnconnect-proxy status
./dist/fnconnect-proxy stop
```

控制接口仅监听随机的本地回环端口，并使用每次启动生成的随机密钥鉴权。管理命令不会按状态文件中的 PID 发送信号，旧状态不会导致误停其他进程。并发启停使用进程间文件锁；后台退出后可再次 `start`。日志按追加方式写入，`logs` 从最近最多 1 MiB 中读取末尾指定行数。启动或停止超时会报告错误和日志路径。

后台进程不自动重启，也不设置开机启动。Linux、macOS 和 Windows 构建均包含 CLI 管理功能。

## 5. 管理

```bash
fnconnect-proxy status
fnconnect-proxy refresh
fnconnect-proxy logout
```

`refresh` 使用保存的 `longToken` 强制获取新的 `fnos-token` 和 `entry-token`；没有可用的 `longToken` 时需要重新登录。`logout` 仅清除本地 token，不修改 NAS 账号。

## 安全

`longToken` 是长期登录凭据，应视同密码级秘密。不要上传 `~/.config/fnconnect-proxy/config.json`，不要粘贴到公开日志。helper 只监听 `127.0.0.1`，不要改成 `0.0.0.0`，除非你明确知道风险。

## v1.0.7

- 直接使用 `start` / `stop` / `restart` / `status` / `logs` 管理后台进程。
- 移除独立 shell 开关；后台运行由 CLI 完成，无需安装系统服务。
- 后台模式优雅停止，并在退出前关闭代理连接和自动刷新任务。

## v1.0.6

- 基于 v1.0.3 的已验证转发行为：`Authorization` 原样穿透，不做私有 Header 改名。
- `/app/...` Cookie 保持已实测可用的形态：`mode=relay; language=zh-CN; fnos-token=...; entry-token=...`。
- 自动刷新每 30 分钟执行，降低对 token 生命周期估计的依赖。
- 旧 `fnos-token` 过期或即将过期时，使用保存的 `longToken` 获取新会话；签名式 `tokenLogin` 沿用已建立的签名密钥，并通过 entry-token 交换验证。
- 网关仍拒绝已刷新 entry-token 的请求时，强制更新 fnos-token 后再重试，总计最多两次。
- 目标地址、网关模式和认证 Cookie 使用同一份配置快照，重新登录切换 NAS 时一起生效。
- 刷新失败的回退使用新的 WebSocket；取消和超时会关闭连接，避免遗留读取线程抢走响应。
- 配置保存使用进程间文件锁、版本比较和独立临时文件，避免刷新覆盖新登录或退出。
- 本地 TLS 模拟验证与真实 NAS 验证范围见 [VALIDATION.md](VALIDATION.md)。
