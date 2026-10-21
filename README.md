# codex-pass

`codex-pass` 是一个仅使用 Go 标准库的 Codex Responses API 本地透明代理。它只提供 `POST /v1/responses` 和 `GET /healthz`，默认监听 `127.0.0.1:8317`。

设计目标是可审计的转发：流式请求和 SSE 内容按字节转发；非流式请求只把顶层 `stream` 改为 `true`，随后从 SSE 中提取 `response.completed.response`。当前有五类显式兼容改写：字符串 `input` 包装为官方 Codex 使用的用户消息列表，顶层 `service_tier: "fast"` 映射为 `"priority"`，顶层 `store` 强制为布尔值 `false`，顶层 `temperature` 和 `prompt_cache_options` 会被删除，并删除任意嵌套位置的 `prompt_cache_breakpoint`。所有改写都会记录在 Debug 字段差异中；其他上游参数错误会原样返回，不会通过删除字段、改模型名或注入工具来“兼容”。

## 构建与测试

需要 Go 1.27.3。

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o codex-pass ./cmd/codex-pass
```

## 启动

```bash
./codex-pass \
  --key sk-local-secret \
  --listen 127.0.0.1:8317 \
  --debug ./proxy.jsonl \
  /path/to/copied-auth.json
```

`--key` 必填，`--debug` 和 `--listen` 可选，后面必须给出至少一个 Auth File。`--listen` 优先于 `CODEX_PROXY_LISTEN`；两者都未设置时默认监听 `127.0.0.1:8317`。如果改成非回环地址，程序会发出 TLS、访问控制和日志保护提示。

建议先复制官方 Codex 或 CPA 的认证文件到专用目录，再传给代理。刷新令牌通常会轮换；代理会原子写回指定文件并设为 `0600`，因此不建议多个程序同时刷新同一个源文件。

支持两类明确格式：

- 官方 Codex：`auth_mode: "chatgpt"`，并且 `tokens` 内有 `access_token`、`refresh_token`、`account_id`。
- 已知 CPA 展平格式：`type: "codex"`，并有 `access_token`（或 `accessToken`）、`refresh_token`（或 `refreshToken`）、`account_id`（也接受 `chatgpt_account_id` / `accountId`）。

其他格式会在启动时以明确错误拒绝，不会猜测字段含义。

## 调用示例

流式请求：

```bash
curl -N http://127.0.0.1:8317/v1/responses \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  -H 'session-id: cache-experiment-1' \
  --data '{
    "model":"gpt-6-luna",
    "stream":true,
    "prompt_cache_key":"cache-experiment-1",
    "input":[{"role":"user","content":[{"type":"input_text","text":"Hello"}]}]
  }'
```

非流式请求：

```bash
curl http://127.0.0.1:8317/v1/responses \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","stream":false,"input":"Hello"}'
```

如果省略 `stream`，代理同样执行非流式聚合。流式请求不会解析、重排或重写上游 SSE；当 `input` 为字符串时，代理将其包装成 `[ { "type": "message", "role": "user", "content": [ { "type": "input_text", "text": "…" } ] } ]`。当 `service_tier` 为 `fast`、`store` 缺失或不为布尔 `false`、顶层出现 `temperature` 或 `prompt_cache_options`、或任意对象含 `prompt_cache_breakpoint` 时，代理也会重编码请求 JSON，执行对应改写，因此这些特例不保持请求体字节一致。`prompt_cache_key` 不受影响。非流式请求会处理多行 `data:`、`\r\n`、keepalive、任意 TCP 分块和大事件，且不使用 `bufio.Scanner`。

## 上游认证与亲和性

上游固定为 `https://chatgpt.com/backend-api/codex/responses`。代理替换客户端的 `Authorization` 和 `ChatGPT-Account-ID`，并保留客户端显式提供的 `session-id`、`thread-id`、`originator` 等非认证头。没有显式 `session-id` 时，若请求包含字符串 `prompt_cache_key`，代理将该值作为上游 `session-id`。

多个 Auth File 不会轮询：相同 `prompt_cache_key` 始终选择同一账号；没有缓存键时，使用 `session-id` 做稳定选择；二者均无时使用第一个账号。OAuth Access Token 是 JWT 时，代理在到期前 5 分钟刷新；同一个 Auth File 的并发刷新会合并为一次请求。若上游在尚未输出前返回 401，会强制刷新后重试一次。

代理使用官方 Codex CLI 源码中可确认的 OAuth refresh JSON grant、`ChatGPT-Account-ID`、`session-id`、`thread-id`、`originator` 和 User-Agent 协议习惯；它不生成或注入 Codex 的私有 turn metadata。

## Debug 日志

`--debug` 输出权限为 `0600` 的 JSON Lines。每条记录都有 `timestamp`、`request_id` 和 `stage`。主要阶段包括：

- `CLIENT_REQUEST`、`AUTH_SELECTION`、`UPSTREAM_REQUEST`、`UPSTREAM_RESPONSE_HEADERS`
- `UPSTREAM_SSE`、`UPSTREAM_COMPLETED`、`CLIENT_RESPONSE`、`REQUEST_SUMMARY`

日志中包含客户端与实际上游 JSON、请求头对照、自动字段差异、账号文件编号、缓存键、实际 session id、时延和上游 Usage。缓存计量会以 `absent`、`null`、`zero`、`positive`（以及异常负数）区分 `input_tokens`、`cached_tokens`、`cache_write_tokens`，从不自行推测。

日志会脱敏 Bearer 凭证、`sk-...`、OAuth Access / Refresh / ID Token；脱敏仅发生在日志副本上，绝不会影响上游请求。日志写入由独立协程串行落盘并通过有界队列施加背压，不会静默丢失记录。

`--key` 会出现在进程参数中。实际使用时可限制本机用户权限，或改由受保护的启动器传入。

## 诊断脚本

脚本不会自行寻找或使用任何凭证。调用者明确传入 Auth File 后才会启动本地代理并向上游发送请求。

准备一个基础 JSON（Developer 前缀至少 1500 tokens）后运行缓存实验：

```bash
scripts/probe_cache.sh \
  --bin ./codex-pass \
  --auth /path/to/copied-auth.json \
  --key sk-local-secret \
  --request ./request.json \
  --model gpt-6-luna \
  --cache-key fixed-translation-key \
  --session-id fixed-translation-key
```

它将同一请求发送三次，保存每次响应和完整 `proxy.jsonl`，并输出包含 HTTP、首字节时间、总时延、Input、Cached、Cache Write 的表格。

Fast / Priority 对照：

```bash
scripts/probe_fast.sh \
  --bin ./codex-pass \
  --auth /path/to/copied-auth.json \
  --key sk-local-secret \
  --request ./request.json \
  --model gpt-6-luna
```

它按 `service_tier` 缺省、`fast`、`priority` 各运行三次，并记录请求值、响应回显、Usage 和时延。当前代理会将 `fast` 显式改写为 `priority`；可在同目录的 `proxy.jsonl` 中查看原始请求、实际上游请求和 `REQUEST_DIFF`。脚本只报告观测结果；响应回显值不被解释为调度行为的充分证据。

## 范围

未提供 Web 管理面板、数据库、用户系统、Chat Completions 转换、模型映射、参数修复、请求内容缓存、负载均衡或 WebSocket。非流式 SSE 未出现 `response.completed` 时，代理返回明确的 502 聚合失败，不会伪造成功结果。
