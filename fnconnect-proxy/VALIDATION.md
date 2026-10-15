# Validation notes — fnconnect-proxy v1.0.6

## Local verification

The tests exercise a local TLS server implementing the signed fnOS WebSocket
requests and an HTTP FN Connect gateway. The production client connects with
certificate verification enabled; a separate test process trusts the fixture's
certificate. No real NAS credentials are used.

`TestLocalTLSRefreshWorkflow` verifies:

- Rejected `user.authToken` falls back to `user.tokenLogin` on a fresh connection.
- Successful WebSocket authentication does not hide HTTP gateway rejection:
  after refreshing entry-token, a second rejection forces fnos-token renewal.
- A disconnected WebSocket is replaced before longToken fallback.
- Explicit manual renewal and renewal near estimated expiry get both new
  fnos-token and new entry-token.
- Persistent gateway rejection stops after two retries and returns 502.
- The gateway accepts the renewed Cookie pair, the original LLM Authorization
  and request body survive retries, and a subsequent request succeeds with the
  persisted session loaded into a new manager.
- An unchanged longToken retains its original estimated expiry.

Other regression tests cover entry-token-only SSE streaming, changing NAS and
route mode on re-login, refresh racing with login/logout, cancellation during
TLS connection and WebSocket reads, LLM 401 responses, and four concurrent
configuration writer processes without lost updates or leftover temporary files.

The signed tokenLogin path retains the established signing key. Its fresh AES
key/IV were never sent to the server and cannot decrypt a returned secret. This
matches the token-login flows in the primary SDK sources:
[fnos-ts-client](https://github.com/Timandes/fnos-ts-client/blob/main/src/client.ts)
and [pyfnos](https://github.com/Timandes/pyfnos/blob/main/fnos/client.py).
The fixture checks HMAC signatures after tokenLogin before issuing entry-token.

Checks passed in this workspace:

```text
fnconnect-proxy: go test -race ./...; go vet ./...
fnos-llmproxy:   go test ./...; go vet ./...
fnconnect-proxy builds: linux/amd64, windows/amd64, darwin/arm64
```

Windows and macOS builds were cross-compiled; runtime tests ran on Linux.

## Standalone switch

`server.sh` replaces the previous service template with an explicit Linux
background process switch. Shell syntax and patch whitespace were checked with
`bash -n server.sh` and `git diff --check`. The user requested no local
installation or use of this switch, so its start/stop commands were not executed.

## Live NAS verification — 2026-10-15

Using the user's real login and a binary built from the current source, the
following checks passed with TLS certificate verification enabled:

- The initial saved session passed the real unified FN Connect gateway.
- Explicit `refresh` completed successfully using longToken, updated entry-token
  and the saved configuration revision, and retained the established signing key.
  The server returned the same fnos-token value for this still-valid session.
- Direct requests using the refreshed saved Cookie pair passed the NAS gateway.
- A running proxy adopted the refreshed configuration, and a restarted proxy
  successfully used the persisted session.
- A separate WebSocket connection authenticated the saved fnos-token and signing
  key and exchanged another entry-token successfully; HTTP requests still passed.
- In an isolated configuration copy, an intentionally invalid fnos-token triggered
  automatic recovery using longToken. The saved fnos-token recovered to the valid
  session value, and two consecutive HTTP requests passed the real gateway.
- A final request with the user's original saved configuration also passed.
  Temporary validation proxy processes and isolated configuration copies were removed.

The live backend returned HTTP 401 JSON with `authentication_error` and a message
requesting a magpie gateway API key. This response comes from the LLM backend and
establishes that the NAS gateway was passed. No LLM API key was available, so a
successful authenticated `/v1/models` response or model invocation was not tested.

This deployment also accepted an intentionally invalid entry-token while the
fnos-token remained valid; that case did not trigger an entry-token refresh. The
live checks therefore establish short-session recovery and explicit entry-token
refresh, without demonstrating gateway rejection of entry-token alone.

One probe using Python's default User-Agent received HTTP 403 `error code: 1010`.
Repeating it with the same explicit User-Agent as the successful baseline passed;
the User-Agent was the changed request field. This response should not be treated
as an fnOS token rejection.

## Scope

The tests simulate rejection immediately, including with real NAS requests. They
do not establish the actual token lifetime on this fnOS version or verify a
multi-hour NAS session across natural expiration.

The health endpoint only checks that the local HTTP service is running; it does
not validate NAS authentication. The `refresh` command renews and stores tokens;
to check the complete deployed route, request `/v1/models` through the local
proxy with a valid LLM API key.
