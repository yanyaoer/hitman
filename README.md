# hitman

**H**uman **I**n **T**he **M**iddle — watch the **A**gent **N**etwork.

`hitman` 是一个被授权的 human-in-the-middle：透明插在本地 agent client 与上游模型后端之间，在不改
agent 客户端配置的前提下，统一观察、审计、路由、重放、折叠续写，并为后续人工审批 / policy gate 留出
介入点。当前实现覆盖 `codex` → `chatgpt.com`，并把 Anthropic / Gemini 官方 API endpoint 纳入同一套
透明接管与审计日志。

当前能力：拦截本机 `codex` / `claude` / `gemini` / `omp` / `pi` / `agy` 等 agent CLI 发往模型 API
的流量，**不改任何 CLI 配置**，在本地解密后审计命中的模型请求，并修复 gpt-5.5 的 **518n−2 推理截断降智**（折叠逻辑移植自
[cpa-plugin-codexcomp](https://github.com/uf-hy/cpa-plugin-codexcomp)），再用**客户端自己的
token / API key** 经 sing-box 转发上游。对 CLI 完全无感——照常敲原命令即可。

## 数据流

```
agent CLI ──TLS(SNI=upstream API host)──▶ sing-box(TUN)
   路由规则: inbound=tun-in ∧ process∈{codex,claude,claude.exe,gemini,omp,pi,agy}
            ∧ domain∈{chatgpt.com,api.anthropic.com,generativelanguage.googleapis.com,aiplatform.googleapis.com}
            → override 到 127.0.0.1:8471
        │
        ▼
hitman (127.0.0.1:8471, 单一 Go 二进制)
   1. 本地 CA 按 SNI 现签上游 host 证书 → 终止 TLS（CLI 经钥匙串信任该 CA）
   2. 仅 POST /backend-api/codex/responses 且命中折叠门 → 折叠；其余路径透明代理
   3. 审计：请求体 + 命中 endpoint 的响应流/响应体落盘（Authorization/API key 等脱敏）
   4. 折叠：检测 518n−2 + encrypted_content 重放续写，多轮折叠成单响应
   5. 上游：复用客户端自己的 Bearer/API key，经 socks5://127.0.0.1:2333 或 direct 直连真上游
        │
        ▼
sing-box socks-in ──(inbound=socks-in，不命中重定向，故不成环)──▶ 节点选择 ──▶ upstream API
```

上游经 socks-in 进入 sing-box、而重定向规则限定 `inbound=tun-in`，因此 hitman 自己的续写请求
不会被再次重定向，不成环。

## 前置条件

- Go 1.26（构建用），macOS arm64。
- sing-box 正在运行（SFM 或 CLI 均可，见下），socks-in 监听 `127.0.0.1:2333`，
  且有一个 TUN 入站（默认 tag `tun-in`）。
- `jq`、`curl`、`/opt/homebrew/bin/sing-box`（`hitman` 脚本使用）。

## sing-box 两种模式（SFM / CLI）

`hitman` 同时支持两种 sing-box 部署，自动探测，也可用环境变量强制：

| 模式 | 配置文件 | reload 方式 |
|---|---|---|
| `sfm`（SFM.app / NetworkExtension） | `~/Library/Group Containers/287TTNZF8L.io.nekohasekai.sfavt/configs/config_2.json` | 在 SFM App 里重连当前 profile（无法脚本化，脚本会提示） |
| `cli`（`sing-box run -c <cfg>`） | `$HITMAN_SB_CONFIG` → `~/.config/sing-box/config.json` → `/opt/homebrew/etc/sing-box/config.json` … | 重启进程；若在 launchd 下用 `HITMAN_SB_LAUNCHD` 指定 label 自动 kickstart |

覆盖用环境变量：`HITMAN_SB_MODE=sfm|cli|auto`、`HITMAN_SB_CONFIG=<path>`、
`HITMAN_SB_RELOAD='<自定义 reload 命令>'`、`HITMAN_SB_LAUNCHD=<launchd label>`、
`HITMAN_TUN_INBOUND=<tun 入站 tag，默认 tun-in>`。`./hitman status` 会显示解析出的模式与配置路径。

> CLI 模式要求你的 sing-box 配置里有 tag 为 `tun-in`（或 `HITMAN_TUN_INBOUND` 指定）的 TUN 入站、
> 以及 `127.0.0.1:2333` 的 socks 入站——否则重定向规则不生效。

## 一次性安装

```bash
./hitman init
```

它依次完成：`build`（编译 `bin/hitman`）→ `install`（装 launchd 常驻，首次启动自动生成本地 CA）
→ `ca-trust`（把 `ca/hitman-ca.pem` 加入系统钥匙串信任，**需输入管理员密码**）
→ `singbox-patch`（幂等插入重定向规则并 `sing-box check`）→ `status` → `smoke`。

launchd label 为 `com.miclaw.hitman`。`install` 会按当前工作副本路径重写 plist 里的二进制、日志和工作目录，
所以仓库目录现在仍叫 `ai-bridge` 或之后改成 `hitman` 都可以直接重新安装。

> 打完 sing-box 规则后需**重载 SFM**（在 App 里重连当前 profile）才会生效；`./hitman on` 只负责写配置和校验。

单独重跑某步：`./hitman build|install|ca-trust|singbox-patch|status|smoke`。

## 日常使用

什么都不用做——正常运行 `codex` / `claude` / `gemini` / `omp` / `pi` / `agy` 即可。要确认接管和折叠生效，
看当天审计摘要：

```bash
tail -f audit/$(date +%F)/index.jsonl     # 每请求一行，folded/rounds/usage/stopped_reason
```

`response.completed` 里会带 `metadata.proxy_rounds`（每轮的 reasoning_tokens 与截断层级 n）。

## 已识别 endpoint

| Provider | Host | Endpoint | 日志处理 |
|---|---|---|---|
| Codex | `chatgpt.com` | `POST /backend-api/codex/responses` | 请求体 + SSE；gpt-5.5 stream 可折叠并记录 `usage` |
| Anthropic | `api.anthropic.com` | `POST /v1/messages`、`POST /v1/messages/count_tokens`、`GET /v1/models` | 请求体 + `.sse`/`.response`；提取 `input_tokens`、`output_tokens`、cache/thinking tokens |
| Gemini | `generativelanguage.googleapis.com` / `*.aiplatform.googleapis.com` | `/v1beta/models/*:{generateContent,streamGenerateContent,countTokens}`、`/v1/models/*:{...}` 与 Vertex `/publishers/google/models/*:{...}` | 请求体 + `.sse`/`.response`；提取 `usageMetadata` 为统一 token 摘要 |

## 逃生舱

```bash
./hitman off     # 移除 sing-box 重定向规则 → agent 直连原 upstream（用它自己的 token/API key，无残留）
./hitman on      # 重新启用
```

hitman 进程若崩溃，launchd `KeepAlive` 会在 ~10s 内自动拉起。

## 审计布局

```
audit/<date>/
  req-<id>.json   # 元数据 + 脱敏请求头 + 请求体
  req-<id>.sse    # 完整下游 SSE 响应流（折叠后或上游 SSE）
  req-<id>.response # 非 SSE endpoint 的完整下游响应体
  index.jsonl     # 每请求一行摘要
```

`Authorization`/`Cookie`/`X-Api-Key`/`Anthropic-Api-Key`/`X-Goog-Api-Key` 等敏感头在落盘时替换为
`***`。请求体默认保留（含 prompt，本机自用）。可用 `HITMAN_AUDIT_BODIES=false` 关闭请求体记录。

## 可调环境变量（默认零配置）

| 变量 | 默认 | 说明 |
|---|---|---|
| `HITMAN_LISTEN` | `127.0.0.1:8471` | MITM 监听地址 |
| `HITMAN_SOCKS` | `127.0.0.1:2333` | 上游出口：socks5 地址，或 `direct`（直连，由 TUN 捕获）|
| `HITMAN_ALLOW_HOSTS` | `chatgpt.com,api.anthropic.com,generativelanguage.googleapis.com,aiplatform.googleapis.com` | 允许转发的上游 Host（逗号分隔，精确或 `.后缀`）；空=不限制 |
| `HITMAN_DOMAINS` | 同上 | 写入 sing-box 重定向规则的域名列表 |
| `HITMAN_DOMAIN_SUFFIXES` | `aiplatform.googleapis.com` | 写入 sing-box 重定向规则的域名后缀列表，用于 Vertex 区域域名 |
| `HITMAN_PROCESSES` | `codex,claude,claude.exe,gemini,omp,pi,agy` | 写入 sing-box 重定向规则的进程名列表；`claude.exe` 覆盖 Claude Code 打包二进制在 SFM 中的进程名；设为空字符串表示 host 级重定向 |
| `HITMAN_MAX_CONTINUE` | `0` | 续写轮数：**默认 0 = 不折叠**（只接管+审计+截断检测）；设 `≥1` 启用多轮折叠 |
| `HITMAN_MAX_TIER_N` | `6` | 允许续写的最大截断层级 |
| `HITMAN_TRUNCATION_STEP` | `518` | 截断检测步长（无新样本证据勿改） |
| `HITMAN_MARKER` | `Continue thinking...` | 续写提示文本 |
| `HITMAN_DEBUG` | `false` | 折叠调试日志 |
| `HITMAN_AUDIT_BODIES` | `true` | 是否记录请求体 |

## 排障

| 现象 | 原因 / 处理 |
|---|---|
| codex 启动时刷一串 `websocket ... Connection reset` / `Reconnecting...` | 正常。codex 默认先试 WebSocket 传输，hitman 只做 HTTP/SSE（才能折叠），会返回 426 让它快速回退到 HTTPS/SSE。有几秒延迟，之后正常。 |
| codex 报证书错误 | codex 默认未认钥匙串根 → 回落 `CODEX_CA_CERTIFICATE=<ca.pem>`（launchd 注入，仍不碰 `config.toml`） |
| 流量没被拦截 | `process_name` 匹配在 SFM 下不生效 → `HITMAN_PROCESSES= ./hitman on` 改为 host 级重定向，hitman 会透传非目标 endpoint 流量 |
| 重定向规则消失 | SFM 更新订阅覆盖了手改 → 重跑 `./hitman on`；`./hitman status` 可检测规则是否在位 |
| hitman 掉线 | launchd 自愈；或 `./hitman off` 回官方直连 |
| socks 2333 连不上（但 SFM 在跑） | sing-box `strict_route: true` 会阻止本机进程连它自己的 socks 入站。改用直连出口：`./hitman egress direct`（hitman 直接拨 upstream，由 TUN 捕获转发） |
| socks 不通 | SFM/TUN 未运行 → 启动 SFM；`./hitman status` 探测 2333 |

## 出口模式（socks / direct）

hitman 到上游有两种走法，`./hitman egress <mode>` 切换（改 plist 并重启服务）：

- `socks`（默认 `127.0.0.1:2333`）：经 sing-box socks 入站出海。**需要该端口本机可连**——但很多 TUN 配置开了
  `strict_route: true`，会让本机进程连不上 socks 入站（表现为连接超时，尽管 `sudo lsof` 能看到在监听）。
- `direct`：hitman 直接拨上游 API host，由 sing-box 的 TUN 捕获并按节点路由。**推荐在 SFM/strict_route 下用**。

> ⚠️ `direct` 出口**仅在按进程重定向（规则含 `process_name`）时安全**：hitman 自身进程不匹配目标 CLI，
> 故不会被回环重定向。若改用 **host 级重定向**（`HITMAN_PROCESSES=`），`direct` 会把 hitman 自己的上游请求也
> 重定向回自己造成**死循环**——那种情况必须用 socks 出口（或在 sing-box 里把 hitman 的流量排除）。

## 致谢

折叠算法派生自 cpa-plugin-codexcomp / codexcomp / CodexCont（均 MIT），详见 [NOTICE](NOTICE)。
