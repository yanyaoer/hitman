# hitman

**H**uman **I**n **T**he **M**iddle - watch the **A**gent **N**etwork.

`hitman` is a local, authorized MITM for agent CLIs. It intercepts selected model
API traffic, decrypts it with a local CA, writes audit logs, and forwards the
request with the client's own credentials through either an explicit proxy or
the system route.

The current implementation covers:

- Codex traffic to `chatgpt.com`, including the gpt-5.5 518n-2 folding path.
- Anthropic traffic to `api.anthropic.com`.
- Gemini traffic to `generativelanguage.googleapis.com` and Vertex Gemini
  `*-aiplatform.googleapis.com`.
- Agent process names: `codex`, `claude`, `claude.exe`, `gemini`, `omp`, `pi`,
  and `agy`.

## Data Flow

```
agent CLI
  │  normal HTTPS request to chatgpt.com / api.anthropic.com / googleapis.com
  ▼
macOS /etc/resolver/<domain>
  │  selected domains use 127.0.0.1:8472
  ▼
hitman netd DNS
  │  target A records become 198.18.0.0/15 fake IPs
  ▼
hitman netd utun + gVisor TCP stack
  │  process_name match? codex/claude/gemini/omp/pi/agy
  ├─ yes: raw TCP pipe to hitman MITM at 127.0.0.1:8471
  └─ no:  raw TCP pass-through via the configured upstream mode
  ▼
hitman MITM
  │  local CA signs SNI cert, audits request/response, optionally folds Codex
  ▼
upstream mode
  ├─ proxy:  socks5/http CONNECT ip:port, e.g. sing-box or mihomo
  └─ system: resolve real IP with upstreamDNS, then let macOS route it
  ▼
upstream model API
```

`hitman` no longer edits SFM or sing-box config. It only requires an existing
proxy if you choose proxy mode.

## Services

Two launchd services are installed:

| Service | Scope | Purpose |
|---|---:|---|
| `com.hitman.srv` | user LaunchAgent | MITM HTTPS server on `127.0.0.1:8471`, audit writer, CA issuer |
| `com.hitman.net` | root LaunchDaemon | fake DNS on `127.0.0.1:8472`, utun, fake-IP route, `/etc/resolver` files |

The root daemon is intentionally narrow: it owns network plumbing. The user
service owns prompt data and audit files.

## Requirements

- macOS.
- Go 1.26.
- One upstream mode:
  - `system`: no proxy; hitman resolves the real upstream IP with `upstreamDNS` and lets macOS route the connection.
  - `proxy`: a reachable SOCKS5 or HTTP CONNECT proxy, such as sing-box or mihomo.
- `curl`, `jq`, `launchctl`, and `/usr/libexec/PlistBuddy`.

No SFM profile rewrite is required. If SFM, sing-box, or mihomo already exposes
a reachable socks/mixed/http inbound, hitman can use it as-is.

## Install

```bash
./hitman init
```

`init` runs:

1. `build`: compiles `bin/hitman` with `-tags with_gvisor`.
2. `install`: installs and starts both launchd services.
3. `ca-trust`: trusts `ca/hitman-ca.pem` in the System keychain.
4. `status`.
5. `smoke`: live DNS/TUN/netd smoke using a temporary process named `codex`.

The CA is created on first MITM startup. If `ca-trust` says the CA is missing,
run `./hitman install`, wait a second, then run `./hitman ca-trust`.

## Daily Commands

```bash
./hitman on              # build + start MITM plus root netd capture
./hitman off             # stop netd and remove hitman-managed resolver files
./hitman status          # services, listeners, proxy, fake route, resolvers, CA
./hitman logs            # tail MITM and netd logs
./hitman smoke           # live end-to-end DNS/TUN/netd smoke
./hitman smoke-mitm      # MITM-only smoke with curl --connect-to
```

Set the upstream mode:

```bash
./hitman upstream system           # default: real-IP dial, then system route
./hitman upstream socks            # socks5://127.0.0.1:2333
./hitman upstream socks 127.0.0.1:1080
./hitman upstream http             # http://127.0.0.1:2334
./hitman upstream http 127.0.0.1:7890
```

In `proxy` mode, the order is explicit:

```
agent-cli -> hitman netd -> hitman MITM -> socks/http proxy -> upstream
```

In `system` mode, hitman does not use macOS resolver for upstream hosts. It asks
`upstreamDNS` for a real A/AAAA record, rejects fake-IP answers inside
`fakeIPCIDR`, dials the real IP, and then macOS routing decides the next
hop. This avoids the hitman fake-DNS loop while allowing the system VPN/TUN route
to carry the connection if one is active.

`./hitman on` is the SFM-free capture switch. It builds the with-gVisor binary,
starts the user MITM LaunchAgent, starts the root `netd` LaunchDaemon, installs
hitman-managed `/etc/resolver/*` files, opens fake DNS on `127.0.0.1:8472`, and
adds the `198.18.0.0/15` fake-IP route through the hitman utun interface. It does
not edit SFM, sing-box, or mihomo config.

## Audit Layout

```
audit/<date>/
  req-<id>.json       # metadata + redacted request headers + optional request body
  req-<id>.sse        # streamed response log
  req-<id>.response   # non-SSE response body
  index.jsonl         # one summary line per request
```

Sensitive headers such as `Authorization`, `Cookie`, `X-Api-Key`,
`Anthropic-Api-Key`, and `X-Goog-Api-Key` are redacted. Request bodies are kept
by default for local audit; disable with:

```json
{
  "auditBodies": false
}
```

## Endpoint Logging

| Provider | Host | Endpoint | Logging |
|---|---|---|---|
| Codex | `chatgpt.com` | `POST /backend-api/codex/responses` | request + SSE; gpt-5.5 stream can fold and records usage |
| Anthropic | `api.anthropic.com` | `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models` | request + `.sse`/`.response`; extracts input/output/cache/thinking tokens |
| Gemini | `generativelanguage.googleapis.com`, `*-aiplatform.googleapis.com` | `generateContent`, `streamGenerateContent`, `countTokens` | request + `.sse`/`.response`; extracts `usageMetadata` |

## Configuration

Runtime configuration is read from:

```text
$HOME/.config/hitman/config.json
```

If the file is missing, hitman uses built-in defaults. The default upstream mode
is `system`, so a fresh install does not require any proxy IP:port. Running
`./hitman upstream socks ...` or `./hitman upstream http ...` writes
`upstreamMode=proxy` plus `upstreamProxy` into this file; running
`./hitman upstream system` removes `upstreamProxy`.

The launchd plists are intentionally kept to process launch metadata only:
binary path, working directory, logs, `HOME`, and `PATH`. They do not store
runtime routing or capture config.

Example:

```json
{
  "upstreamMode": "system",
  "upstreamDNS": "1.1.1.1:53",
  "processes": ["codex", "claude", "claude.exe", "gemini", "omp", "pi", "agy"]
}
```

Proxy example:

```json
{
  "upstreamMode": "proxy",
  "upstreamProxy": "socks5://127.0.0.1:2333"
}
```

| JSON key | Default | Used By | Meaning |
|---|---|---|---|
| `listen` | `127.0.0.1:8471` | MITM | HTTPS MITM listener |
| `upstreamMode` | `system` | MITM + netd | `system` or `proxy`; `direct` is accepted as an alias for `system` |
| `upstreamProxy` | empty | MITM + netd | SOCKS5 or HTTP CONNECT proxy address in `proxy` mode; if set without `upstreamMode`, mode is inferred as `proxy` |
| `upstreamDNS` | `1.1.1.1:53` | MITM + netd | DNS upstream for non-target resolver-zone names and real-IP resolution in `system` mode |
| `caDir` | `ca` | MITM + script | local CA directory |
| `auditDir` | `audit` | MITM | per-day audit output directory |
| `allowHosts` | target hosts | MITM | defense-in-depth upstream Host allowlist |
| `netDNS` | `127.0.0.1:8472` | netd + script | fake DNS listener |
| `mitmAddr` | `127.0.0.1:8471` | netd | MITM listener that matching flows are piped into |
| `fakeIPCIDR` | `198.18.0.0/15` | MITM + netd | fake-IP pool, TUN route, and system-mode loop guard |
| `tunAddress` | `172.31.255.1/30` | netd | utun interface address |
| `tunName` | empty | netd | optional utun name hint |
| `domains` | `chatgpt.com,api.anthropic.com,generativelanguage.googleapis.com,aiplatform.googleapis.com` | netd | exact fake-DNS targets |
| `domainSuffixes` | `aiplatform.googleapis.com` | netd | suffix fake-DNS targets; matches Vertex region hosts |
| `resolverDomains` | `chatgpt.com,anthropic.com,googleapis.com` | netd + script | `/etc/resolver/<domain>` files to manage |
| `processes` | `codex,claude,claude.exe,gemini,omp,pi,agy` | netd | process basenames allowed into MITM |
| `processPaths` | empty | netd | exact process paths allowed into MITM |
| `markerText` | `Continue thinking...` | MITM | Codex folding marker |
| `truncationStep` | `518` | MITM | Codex fold truncation step |
| `maxTierN` | `6` | MITM | Codex fold tier cap |
| `maxContinue` | `0` | MITM | Codex fold continuation rounds; `0` means audit only |
| `debug` | `false` | MITM | verbose debug logging |
| `auditBodies` | `true` | MITM | write request bodies |

`HITMAN_*` environment variables are still accepted as transient overrides for
development and one-off launches, but the control script no longer writes them
into launchd plists. The legacy `HITMAN_SOCKS` variable is still accepted as a
fallback for `upstreamProxy`.

## Files Owned By hitman

`netd` writes resolver files only when the file is missing or already contains
the marker `# hitman managed resolver`:

```
/etc/resolver/chatgpt.com
/etc/resolver/anthropic.com
/etc/resolver/googleapis.com
```

`./hitman off` and daemon shutdown remove only files containing that marker.
Existing resolver files without the marker are left untouched and cause startup
to fail instead of being overwritten.

## Troubleshooting

| Symptom | Check |
|---|---|
| No Anthropic/Gemini audit lines | `./hitman status`; confirm netd is loaded, DNS `:8472` is listening, resolver files exist, and process name is in `processes`. |
| Agent reports certificate error | Run `./hitman ca-trust`, or pass `NODE_EXTRA_CA_CERTS=$PWD/ca/hitman-ca.pem` for Node-based CLIs. |
| `netd` keeps restarting | `./hitman logs`; common causes are missing `with_gvisor` build, existing non-hitman `/etc/resolver/*` files, or unreachable privileges. |
| Upstream calls fail after MITM | `./hitman status`; in `proxy` mode confirm `upstreamProxy` points at a reachable socks/http proxy; in `system` mode confirm `upstreamDNS` can resolve real upstream IPs and system routing is available. |
| Some googleapis domains break | Non-target names under `googleapis.com` are forwarded to `upstreamDNS`; set that to a DNS server reachable on your network. |
| Want to disable capture immediately | `./hitman off`; this stops netd and removes hitman-managed resolver files. The MITM service can stay running harmlessly. |

## Development

```bash
go test -count=1 ./...
go build -tags with_gvisor -trimpath -o bin/hitman .
bash -n hitman
```

The normal `go test` path does not start TUN or touch `/etc/resolver`. Live
network smoke requires the installed services:

```bash
./hitman on
./hitman smoke
```

`smoke` compiles a temporary client whose process basename is `codex`, sends a
request through normal system DNS, and passes when the DNS/TUN/netd/MITM path
returns an HTTP response. The upstream status code is printed because the check
is for network interception, not model generation.

## Credits

The folding algorithm is derived from cpa-plugin-codexcomp / codexcomp /
CodexCont (MIT); see [NOTICE](NOTICE).
