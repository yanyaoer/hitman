# ai-bridge

透明 MITM 网关：拦截本机 `codex` CLI 发往 `chatgpt.com` 的流量，**不改任何 CLI 配置**，
在本地解密后审计每一个请求，并修复 gpt-5.5 的 **518n−2 推理截断降智**（折叠逻辑移植自
[cpa-plugin-codexcomp](https://github.com/uf-hy/cpa-plugin-codexcomp)），再用**客户端自己的
token** 经 sing-box 转发上游。对 codex 完全无感——照常敲 `codex` 即可。

## 数据流

```
codex ──TLS(SNI=chatgpt.com)──▶ sing-box(TUN)
   路由规则: inbound=tun-in ∧ process=codex ∧ domain=chatgpt.com
            → override 到 127.0.0.1:8471
        │
        ▼
ai-bridge (127.0.0.1:8471, 单一 Go 二进制)
   1. 本地 CA 按 SNI 现签 chatgpt.com 证书 → 终止 TLS（codex 经钥匙串信任该 CA）
   2. 仅 POST /backend-api/codex/responses 且命中折叠门 → 折叠；其余路径透明代理
   3. 审计：请求体 + 响应 SSE 全量落盘（Authorization 等脱敏）
   4. 折叠：检测 518n−2 + encrypted_content 重放续写，多轮折叠成单响应
   5. 上游：复用 codex 自己的 Bearer，经 socks5://127.0.0.1:2333 直连真 chatgpt.com
        │
        ▼
sing-box socks-in ──(inbound=socks-in，不命中重定向，故不成环)──▶ 节点选择 ──▶ chatgpt.com
```

上游经 socks-in 进入 sing-box、而重定向规则限定 `inbound=tun-in`，因此 ai-bridge 自己的续写请求
不会被再次重定向，不成环。

## 前置条件

- Go 1.26（构建用），macOS arm64。
- sing-box / SFM 正在运行，socks-in 监听 `127.0.0.1:2333`（本仓库的 `config_2.json` 已具备）。
- `jq`、`curl`、`/opt/homebrew/bin/sing-box`（`bridge` 脚本使用）。

## 一次性安装

```bash
./bridge init
```

它依次完成：`build`（编译 `bin/ai-bridge`）→ `install`（装 launchd 常驻，首次启动自动生成本地 CA）
→ `ca-trust`（把 `ca/ai-bridge-ca.pem` 加入系统钥匙串信任，**需输入管理员密码**）
→ `singbox-patch`（幂等插入重定向规则并 `sing-box check`）→ `status` → `smoke`。

> 打完 sing-box 规则后需**重载 SFM**（在 App 里重连当前 profile）才会生效。

单独重跑某步：`./bridge build|install|ca-trust|singbox-patch|status|smoke`。

## 日常使用

什么都不用做——正常运行 `codex` 即可。要确认折叠生效，看当天审计摘要：

```bash
tail -f audit/$(date +%F)/index.jsonl     # 每请求一行，folded/rounds/usage/stopped_reason
```

`response.completed` 里会带 `metadata.proxy_rounds`（每轮的 reasoning_tokens 与截断层级 n）。

## 逃生舱

```bash
./bridge off     # 移除 sing-box 重定向规则 → codex 直连 chatgpt.com（用它自己的 token，无残留）
./bridge on      # 重新启用
```

bridge 进程若崩溃，launchd `KeepAlive` 会在 ~10s 内自动拉起。

## 审计布局

```
audit/<date>/
  req-<id>.json   # 元数据 + 脱敏请求头 + 请求体
  req-<id>.sse    # 完整下游响应流（折叠后）
  index.jsonl     # 每请求一行摘要
```

`Authorization`/`Cookie`/`X-Api-Key` 等敏感头在落盘时替换为 `***`。请求体默认保留（含 prompt，
本机自用）。可用 `AI_BRIDGE_AUDIT_BODIES=false` 关闭请求体记录。

## 可调环境变量（默认零配置）

| 变量 | 默认 | 说明 |
|---|---|---|
| `AI_BRIDGE_LISTEN` | `127.0.0.1:8471` | MITM 监听地址 |
| `AI_BRIDGE_SOCKS` | `127.0.0.1:2333` | 上游出口 socks5 |
| `AI_BRIDGE_MAX_CONTINUE` | `3` | 最多续写轮数；`0` 关闭折叠（A/B 基线） |
| `AI_BRIDGE_MAX_TIER_N` | `6` | 允许续写的最大截断层级 |
| `AI_BRIDGE_TRUNCATION_STEP` | `518` | 截断检测步长（无新样本证据勿改） |
| `AI_BRIDGE_MARKER` | `Continue thinking...` | 续写提示文本 |
| `AI_BRIDGE_DEBUG` | `false` | 折叠调试日志 |

## 排障

| 现象 | 原因 / 处理 |
|---|---|
| codex 报证书错误 | codex 默认未认钥匙串根 → 回落 `CODEX_CA_CERTIFICATE=<ca.pem>`（launchd 注入，仍不碰 `config.toml`） |
| 流量没被拦截 | `process_name` 匹配在 SFM 下不生效 → 去掉规则里的 `process_name`（改为 host 级重定向），ai-bridge 会透传非 codex 流量 |
| 重定向规则消失 | SFM 更新订阅覆盖了手改 → 重跑 `./bridge on`；`./bridge status` 可检测规则是否在位 |
| bridge 掉线 | launchd 自愈；或 `./bridge off` 回官方直连 |
| socks 不通 | SFM/TUN 未运行 → 启动 SFM；`./bridge status` 探测 2333 |

## 致谢

折叠算法派生自 cpa-plugin-codexcomp / codexcomp / CodexCont（均 MIT），详见 [NOTICE](NOTICE)。
