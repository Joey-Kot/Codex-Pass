# codex-pass

`codex-pass` 是一个 Codex Responses API 本地代理。代理逻辑使用 Go 标准库；可选的 SQLite 请求统计使用纯 Go SQLite 驱动，不要求 CGO 或系统 SQLite 库。它提供 `POST /v1/responses`、`POST /v1/chat/completions` 和 `GET /healthz`，默认监听 `127.0.0.1:8317`。

`/v1/responses` 的设计目标是可审计的转发：流式请求和 SSE 内容按字节转发；非流式请求只把顶层 `stream` 改为 `true`，随后从 SSE 中提取 `response.completed.response`。`/v1/chat/completions` 会将 Chat 请求的 `messages` 转为 Responses `input`，再将上游 Responses 结果转为 Chat Completion：流式请求输出 Chat Completion SSE 块并以 `[DONE]` 结束，非流式请求输出一个 `chat.completion` JSON 对象。当前有五类显式兼容改写：字符串 `input` 包装为官方 Codex 使用的用户消息列表，顶层 `service_tier: "fast"` 映射为 `"priority"`，顶层 `store` 强制为布尔值 `false`，顶层 `temperature` 和 `prompt_cache_options` 会被删除，并删除任意嵌套位置的 `prompt_cache_breakpoint`。所有改写都会记录在 Debug 字段差异中；其他上游参数错误会原样返回，不会通过删除字段、改模型名或注入工具来“兼容”。

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
  --log ./usage \
  /path/to/copied-auth.json
```

常规代理模式下，`--key` 必填，`--debug`、`--log` 和 `--listen` 可选，后面必须给出至少一个 Auth File。`--listen` 优先于 `CODEX_PROXY_LISTEN`；两者都未设置时默认监听 `127.0.0.1:8317`。如果改成非回环地址，程序会发出 TLS、访问控制和日志保护提示。

建议先复制官方 Codex 或 CPA 的认证文件到专用目录，再传给代理。刷新令牌通常会轮换；代理会原子写回指定文件并设为 `0600`，因此不建议多个程序同时刷新同一个源文件。

支持两类明确格式：

- 官方 Codex：`auth_mode: "chatgpt"`，并且 `tokens` 内有 `access_token`、`refresh_token`、`account_id`。
- 已知 CPA 展平格式：`type: "codex"`，并有 `access_token`（或 `accessToken`）、`refresh_token`（或 `refreshToken`）、`account_id`（也接受 `chatgpt_account_id` / `accountId`）。

其他格式会在启动时以明确错误拒绝，不会猜测字段含义。

## 请求用量 SQLite 日志与汇总

传入 `--log DIR` 后，代理会在目录中创建并写入 `DIR/codex-pass.sqlite`。未传该参数时不会创建或写入 SQLite 数据库。

数据库记录每一条已通过本地 API Key 校验的 `/v1/responses` 和 `/v1/chat/completions` 请求，不记录 `/healthz`、未知路径或未认证流量。每条记录包含：请求开始、结束时间，模型名，输入 Token、缓存写入 Token、缓存读取 Token、输出 Token、思考 Token，以及请求中的 `reasoning.effort`（Chat 请求也兼容 `reasoning_effort`）。请求正文、响应正文、认证信息和缓存键不会写入 SQLite。

模型优先使用上游最终响应中的模型名；请求在取得最终响应前失败时，使用客户端请求中的模型名。未由上游返回的用量字段会保存为 `NULL`，汇总只累加上游实际提供的数字，不会推断为零。

请求结果分为以下互斥状态：

- 成功：上游正常完成 `response.completed`。
- 取消：客户端请求上下文已取消，或上游最终状态为 `canceled` / `cancelled`。
- 失败：未正常完成且不属于取消，例如上游错误、网络中断、SSE 提前结束、响应解析失败或转换失败。
- 未结束：进程异常停止前已经开始、但尚未写入最终状态的记录。

使用 `--summary` 查询数据库，不会启动代理，也不要求 `--key` 或 Auth File：

```bash
./codex-pass --log ./usage --summary
./codex-pass --log ./usage --summary --time 2026-06-09,2026-10-10
./codex-pass --log ./usage --summary --time 2026-06-09-10-00,2026-10-10-08-45
./codex-pass --log ./usage --summary --time 2d
./codex-pass --log ./usage --summary --human
```

汇总会输出全部请求的成功、失败、取消、未结束次数，以及输入、缓存写入、缓存读取、输出、思考 Token；同一组指标也会按模型分别输出。`--summary` 指向不存在的数据库会报错，避免目录拼写错误时得到空统计。

`--human` 仅可与 `--summary` 一起使用，会将两张汇总表中的数字按紧凑单位显示，最多保留两位小数，例如 `1.27K`、`2.41M`、`2.34B`。

`--time` 仅可与 `--summary` 一起使用，按请求开始时间过滤：

- `YYYY-MM-DD,YYYY-MM-DD`：按运行机器的本地时区解释，包含首日和末日的整天。
- `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM`：精确到分钟，包含结束分钟。
- `2d`、`1w`、`3m`、`1y`：从执行命令时刻向前回溯到当前时刻；`m` 和 `y` 分别按日历月和日历年计算。

数据库内部使用 UTC Unix 毫秒保存请求时间。SQLite 启用 WAL，因此代理运行时可以执行独立的 `--summary` 查询。

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

Chat Completions 请求示例：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{
    "model":"gpt-6-luna",
    "messages":[{"role":"user","content":"Hello"}]
  }'
```

在相同请求中设置 `"stream": true` 可获得 `text/event-stream` 格式的 Chat Completion 块。`stream_options.include_usage: true` 会在结束块后、`[DONE]` 前增加一个空 `choices` 且带 `usage` 的标准 Usage 块。Chat 输入支持 `system`、`developer`、`user`、`assistant`、`tool` 消息，文本、图片、音频内容块和函数调用；`n` 仅支持 `1`。输出会拼接 Responses 的 `output_text`，并将 `function_call` 映射为 Chat 的 `tool_calls`。

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

未提供 Web 管理面板、用户系统、模型映射、参数修复、请求内容缓存、负载均衡或 WebSocket。`--log` 仅提供本地 SQLite 用量统计，不提供 Web 管理界面。非流式 SSE 未出现 `response.completed` 时，代理返回明确的 502 聚合失败，不会伪造成功结果。
