# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`webtransport-go` is a Go implementation of the [WebTransport over HTTP/3](https://datatracker.ietf.org/doc/draft-ietf-webtrans-http3/) protocol, built on top of [quic-go](https://github.com/quic-go/quic-go). The library exposes `Server` (server-side `Upgrade` handler) and `Dialer` (client) APIs. The package targets the latest two Go releases (currently 1.25 and 1.26).

## Commands

```bash
# Run all tests (uses TIMESCALE_FACTOR in CI to slow timing-sensitive tests)
go test ./...

# Run a single test
go test -run TestSessionCloseWithError -v
go test -run 'TestSession.*' -v

# Race detector (recommended given heavy concurrency)
go test -race ./...

# Coverage
go test -cover -coverprofile=coverage.txt ./...

# Lint — version pinned to v2.9.0 in CI
golangci-lint run --timeout=3m

# Format (gofmt + gofumpt + goimports are required by .golangci.yml)
gofumpt -w .
goimports -w .

# Verify go.mod is tidy (CI enforces this)
go mod tidy -diff

# Build interop binaries
go build -o interopserver       interop_chrome/main.go
go build -o server interop/server/main.go
go build -o client interop/client/main.go
```

When tests are timing-sensitive, CI sets `TIMESCALE_FACTOR=10`. Some tests use Go's `testing/synctest` package — `test_helpers_test.go` deliberately issues TLS certs valid 1990–2100 to survive the synthetic clock.

## Architecture

The package layers a WebTransport session on top of a single QUIC connection. Multiple WebTransport sessions can share one QUIC connection, and WebTransport streams are multiplexed alongside ordinary HTTP/3 streams on the same connection.

**Stream demultiplexing** (`server.go`, `client.go`): every accepted QUIC stream is peeked for its first varint. If it equals `webTransportFrameType` (0x41) for bidi or `webTransportUniStreamType` (0x54) for uni, the next varint is the session ID and the stream is handed to `sessionManager`. Otherwise the stream is forwarded to the underlying `http3` connection (`HandleRequestStream` / `HandleUnidirectionalStream` / `HandleBidirectionalStream`). This is why `ConfigureHTTP3Server` must be called on the `http3.Server` before serving — it stashes the raw `*quic.Conn` in the request context so `Upgrade` can locate it.

**`sessionManager`** (`session_manager.go`): maps `sessionID → *Session`, but also holds an "unestablished" entry that buffers streams arriving *before* the corresponding `CONNECT` request completes (reordering). Buffered streams are released to the session in `AddSession`, or reset with `WT_BUFFERED_STREAM_REJECTED` after `ReorderingTimeout` (default 5s). Recently-closed session IDs are tracked (cap = `maxRecentlyClosedSessions`) to fast-reject late-arriving streams.

**`Session`** (`session.go`): owns the CONNECT stream and parses WebTransport capsules from it (currently only `WT_CLOSE_SESSION`, type 0x2843). Maintains accept queues (`bidiAcceptQueue`, `uniAcceptQueue`) and a `streamsMap` so a session-level close can cancel every stream with `WTSessionGoneErrorCode`. Stream headers (frame type + session ID varint) are precomputed once per session and prepended on the first write of each outgoing stream.

**Streams** (`stream.go`): `SendStream` / `ReceiveStream` / `Stream` wrap the underlying quic-go types. The send side lazily writes the WebTransport stream header on first `Write`; if `Close`/`CancelWrite` is called before the header has been flushed (e.g. blocked on flow control), the header is sent on a background goroutine to keep those calls non-blocking. After the header is written, `SetReliableBoundary()` is called so the QUIC layer can deliver everything before that point reliably even on reset.

**Error code mapping** (`errors.go`): WebTransport stream error codes are remapped to/from a reserved HTTP/3 error code range via `webtransportCodeToHTTPCode` / `httpCodeToWebtransportCode` (the formula skips greasing values inside the range). `WTSessionGoneErrorCode` is intercepted on `Read`/`Write` and converted into `os.ErrDeadlineExceeded` or the session's close error after waiting for the CONNECT stream to close — see `handleSessionGoneError`.

**Protocol versioning** (`protocol.go`): the server advertises both `SETTINGS_ENABLE_WEBTRANSPORT` (draft ≤ 06, 0x2b603742) and `SETTINGS_WT_ENABLED` (draft ≥ 15, 0x2c7cf000) for backwards compatibility. The client only checks the new setting.

**Application protocol negotiation**: the `WT-Available-Protocols` request header (RFC 8941 sf-list) and `WT-Protocol` response header (sf-item) implement the protocol negotiation from draft-14 §3.3. `httpsfv` is used for structured-field encoding.

## Required QUIC config

WebTransport requires QUIC features that are not on by default in quic-go:

- `EnableDatagrams = true`
- `EnableStreamResetPartialDelivery = true`

Both server (`Server.Serve` sets these on the cloned `quic.Config`) and client (`Dialer.Dial` validates them) enforce this. Tests that build a custom `quic.Config` need to set both.

## Interop

- `interop/` — server/client used by the [QUIC Interop Runner](https://interop.seemann.io). Built into a Docker image via `interop/Dockerfile` (workflow: `build-interop-docker.yml`).
- `interop_chrome/` — a Go server plus Selenium-driven Chrome client (`interop.py`) that runs against real Chromium in CI (`interop.yml`). Useful as a smoke test against a non-Go peer.

## Project conventions

- `math/rand/v2` only — `math/rand` and `golang.org/x/exp/rand` are banned by `depguard`.
- Linters enforced: `staticcheck`, `govet`, `unused`, `unparam`, `prealloc`, `exhaustive`, `misspell`, `nolintlint`, `usetesting`, `copyloopvar`, `ineffassign`, `unconvert`, `asciicheck`. Two narrow `staticcheck` exclusions exist (`SA1019` for `quic.ConnectionTracingID/Key`, `SA1029` in test files).
- Files containing `//go:build ignore` are rejected by CI — don't use that tag to park code.
- Tests live alongside code as `*_test.go`. The cross-cutting integration suite is `webtransport_test.go`; per-component suites mirror their files (`session_test.go`, `stream_test.go`, …). `test_helpers_test.go` provides shared TLS material and `NewWebTransportRequest`.

## Debugging / browser interop testing

### `wtscan/` — local debug tool

Single-binary debug/inspect tool for iterating on protocol behavior without rebuilding `interop_chrome/` each round. Two listeners: HTTP UI on `localhost:8080`, WebTransport echo at `https://localhost:12345/echo`. The page pins the server cert via `serverCertificateHashes` so there's no system-trust prompt. The UI follows OS color scheme via `prefers-color-scheme`.

```bash
wtscan/bin/build   # → ./wtscan/bin/wtscan (gitignored)
wtscan/bin/start   # stops anything stale, rebuilds, runs in foreground
wtscan/bin/stop    # kills any wtscan/wtdebug by process name
```

Note: default URL in the page is `https://127.0.0.1:12345/echo`, not `localhost` — Chrome resolves `localhost` to `::1` first and the server binds IPv4-only. `GET /info` on the UI server returns a JSON snapshot of the last observed CONNECT (`:method`, `:protocol`, `:authority`, headers, etc.) — primary way to check what the wire actually carried.

### Chrome via Chrome DevTools MCP

The `mcp__chrome-devtools__*` tools drive a real Chrome over CDP. Useful pattern against `wtscan`:

- `navigate_page` → `http://localhost:8080/`
- `click` the Connect button (use `take_snapshot` first to get element uids)
- `evaluate_script` to read back `transport.reliability`, `getStats()`, or run a one-off `new WebTransport(...).ready` from the page console
- `list_console_messages` after a failed handshake to surface `WebTransportError` details
- `list_network_requests` shows the HTTP/3 CONNECT (Chrome surfaces the `:protocol` value here)

### Safari Technology Preview via AppleScript / `osascript`

Use **Safari Technology Preview** (separate app from stable Safari) — it usually ships WebTransport spec changes earlier, which is the point of testing here.

Prerequisite (one-time, on the machine): Safari Technology Preview → Develop → **Allow JavaScript from Apple Events** must be enabled. Without it, `do JavaScript` returns `"JavaScript through AppleScript is not allowed"`. The toggle is separate from the same-named option in stable Safari.

Open a URL and execute JS:

```bash
osascript -e 'tell application "Safari Technology Preview" to open location "http://localhost:8080/"'
osascript -e 'tell application "Safari Technology Preview" to do JavaScript "document.getElementById(\"url-input\").value" in current tab of window 1'
```

Return values: the value of the final JS expression is printed to stdout by `osascript`. For polling (e.g. wait for `#status` to become `"connected"`), run the `do JavaScript` invocation in a shell loop with `sleep 0.5`. For complex returns, stringify with `JSON.stringify(...)` so the AppleScript bridge doesn't mangle types.

Empirical note: stable Safari 26.5 was observed sending `:protocol = "webtransport"` (legacy token). STP may differ (newer WebKit) — always confirm via `wtscan`'s `/info` panel rather than assuming.
