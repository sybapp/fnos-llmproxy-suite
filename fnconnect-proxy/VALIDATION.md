# Validation — fnconnect-proxy v1.0.5

- `go test ./...`
- `go vet ./...`
- linux/amd64 build
- linux/arm64 build
- unified gateway Cookie shape remains: `mode=relay; language=zh-CN; fnos-token=...; entry-token=...`
- incoming LLM `Authorization` remains unchanged (same behavior as v1.0.3)
- automatic/forced entry refresh never replaces `fnos-token` or uses `longToken`
- exact unified-gateway `200 text/plain invalid token` is detected and triggers one safe entry-token refresh; if still rejected, the proxy asks for a fresh `login` instead of rotating the fnos session
