# Validation — v1.2.0

## Go

- `go test ./...`: PASS
- `go vet ./...`: PASS
- linux/amd64: static x86-64 ELF: PASS
- linux/arm64: static aarch64 ELF: PASS

## Proxy behavior

- HTTP upstream: PASS
- HTTPS upstream / TLS / SNI: retained from v1.1.0
- upstream Host rewrite: PASS
- Authorization preservation: PASS
- FN Connect `entry-token` removal before public upstream: PASS
- non-entry cookies preserved: PASS
- upstream base-path joining: PASS
- SSE streaming / immediate flush: PASS
- direct TCP `:13430` path remains unchanged: PASS

## fnOS unified gateway

- `micro_app=true`: PASS
- `gatewaySocket=app.sock`: PASS
- `gatewayPrefix=/app/llmproxy`: PASS
- Unix domain socket listener: PASS
- socket mode `0660`: PASS
- `/app/llmproxy/v1/responses` -> upstream `/v1/responses`: PASS
- `/v1/models` direct TCP -> upstream `/v1/models`: PASS
- real binary Unix socket + SSE integration test: PASS

Observed integration routing:

```text
Unix socket request:
/app/llmproxy/v1/responses
Authorization: Bearer TEST
Cookie: entry-token=EDGE; keep=yes

Upstream received:
/v1/responses
Authorization: Bearer TEST
Cookie: keep=yes
```

## FPK

- x86 FPK SHA256 sidecar: PASS
- ARM FPK SHA256 sidecar: PASS
- manifest checksum == MD5(app.tgz): PASS
- `service_port=13430` + `micro_app=true`: PASS
- app/ui/config JSON: PASS
- wizard/install JSON: PASS
- wizard/config JSON: PASS
- config/resource JSON: PASS
- config/privilege JSON: PASS

## Not yet physically verified

The generated FPK has not been installed on the user's fnOS 1.2.0 in this build environment. The remaining real-device checks are:

1. upgrade 1.1.0 -> 1.2.0;
2. confirm `/var/apps/llmproxy/target/app.sock` is created;
3. confirm `https://<FN_ID>.fnos.net/app/llmproxy/...` reaches the socket;
4. confirm FN Connect `entry-token` grants access through the main-domain unified gateway.
