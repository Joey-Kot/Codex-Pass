# codex-pass

`codex-pass` 是一个 Codex Responses API 本地代理。代理逻辑使用 Go 标准库；可选的 SQLite 请求统计使用纯 Go SQLite 驱动，不要求 CGO 或系统 SQLite 库。它提供 `POST /v1/responses`、`POST /v1/chat/completions`、`GET /v1/models` 和 `GET /healthz`，默认监听 `127.0.0.1:8317`。

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

使用 `sum` 子命令查询数据库，不会启动代理，也不要求 `--key` 或 Auth File：

```bash
./codex-pass sum --log ./usage
./codex-pass sum --log ./usage --time 2026-06-09,2026-10-10
./codex-pass sum --log ./usage --time 2026-06-09-10-00,2026-10-10-08-45
./codex-pass sum --log ./usage --time 2m
./codex-pass sum --log ./usage --time 1h
./codex-pass sum --log ./usage --time 2d
./codex-pass sum --log ./usage --time 1M
./codex-pass sum --log ./usage --human
./codex-pass --log ./usage --time 1h sum --human
```

`--log`、`--time`、`--human`、`--price` 是公共参数，可放在 `sum` 或 `req` 前后；重复指定时以后面的值为准。两个查询命令都必须提供 `--log DIR`。`sum` 暂无专属参数，`req` 的专属排序和数量参数见下文。使用 `./codex-pass --help` 查看全部命令和参数，使用 `./codex-pass sum --help` 或 `./codex-pass req --help` 查看对应命令帮助。

汇总会输出全部请求的成功、失败、取消、未结束次数，以及输入、缓存写入、缓存读取、输出、思考 Token；同一组指标也会按模型分别输出。`sum` 或 `req` 指向不存在的数据库会报错，避免目录拼写错误时得到空统计。

`--time` 和 `--human` 用于汇总与请求明细查询。`--human` 会将请求次数和 Token 数按紧凑单位显示，最多保留两位小数，例如 `1.27K`、`2.41M`、`2.34B`；请求明细中括号内外的 Token 数都会转换。

`--time` 按请求开始时间过滤：

- `YYYY-MM-DD,YYYY-MM-DD`：按运行机器的本地时区解释，包含首日和末日的整天。
- `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM`：精确到分钟，包含结束分钟。
- `2m`、`1h`、`2d`、`1w`、`3M`、`1y`：从执行命令时刻向前回溯到当前时刻，数量必须为正整数。单位区分大小写：`m` 表示分钟，`h` 表示小时，`d` 表示天（24 小时），`w` 表示周（7 天），`M` 和 `y` 分别按日历月和日历年计算。`1m` 表示最近 1 分钟，`1M` 表示最近 1 个月；原来表示月份的小写 `m` 请改为大写 `M`。

使用 `req` 查看时间范围内的每一条请求：

```bash
./codex-pass req --log ./usage --time 1h --human
./codex-pass --log ./usage --time 2m req --asc
./codex-pass req --log ./usage --time 1M --desc --human
./codex-pass req --log ./usage --num 20
./codex-pass req --log ./usage --time 1h --num 50 --human
```

`req` 输出两张英文表格：第一张 `Request Counts` 包含实际选中请求的全局及按模型统计，不限制条数时与同一时间范围的 `sum` 请求次数表一致；第二张 `Request Logs` 列出以下字段：

| 列名 | 内容 |
| --- | --- |
| `Request time` | 将开始时间的 Unix 毫秒时间戳转换为运行机器本地时间，格式为 `YYYY-MM-DD HH:mm:ss`。 |
| `Time taken` | 结束时间减开始时间；不足 1 分钟显示秒，否则显示分钟，均最多保留 2 位小数，如 `1.45 seconds`、`1.45 minutes`。 |
| `Model` | 模型名，未知时显示 `(unknown)`。 |
| `Input` | 输入 Token 数，括号内为缓存命中 Token 数，如 `56.51K ( 49.52K )`。 |
| `Output` | 输出 Token 数，括号内为思考 Token 数，如 `500 ( 200 )`。 |
| `Cache hit` | 缓存命中 Token 数 ÷ 输入 Token 数，以百分比显示，保留 2 位小数。 |

默认按请求开始时间降序排列，最近的请求在前。`req --asc` 改为升序，`req --desc` 显式指定降序；这两个专属参数放在 `req` 后面，不能同时启用。时间戳相同时按请求 ID 确定顺序。

`req --num N` 最多输出 N 条请求，N 必须为正整数，此专属参数放在 `req` 后面。查询先按 `--time` 筛选，再按指定顺序取前 N 条；默认取最近 N 条，配合 `--asc` 时取最早 N 条。只指定 `--num`、不指定 `--time` 时，默认从全部日志中选取最近 N 条。匹配记录不足 N 条时全部输出；未指定 `--num` 时不限制条数，未指定 `--time` 时不限制时间范围。

指定 `--num` 后，`Request Counts`、模型费用和 `Sum` 金额均只统计实际选中的请求，报告标题会显示数量上限。

未结束请求的耗时、缺失的 Token 数，以及输入 Token 为零或数据缺失时的缓存命中率显示 `-`。没有匹配请求时，次数表显示零，日志表保留表头。

数据库内部使用 UTC Unix 毫秒保存请求时间。SQLite 启用 WAL，因此代理运行时可以执行独立的 `sum` 或 `req` 查询。`req` 的两张表从同一数据库快照读取，确保请求次数与明细一致。

### 价格表与费用估算

传入 `--price FILE`，按指定 JSON 价格表计算所选时间范围的费用：

```bash
./codex-pass sum --log ./usage --time 1M --price ./prices.example.json --human
./codex-pass --price ./prices.example.json req --log ./usage --time 1h --human
```

价格表格式见 [prices.example.json](prices.example.json)，示例使用用户提供的数值。必填字段为 `currency`、`unit`、`service_tier`、`context_threshold`、`models`。目前 `unit` 支持 `per_token`；`context_threshold` 必须为正整数。每个模型都需提供 `short_context`、`long_context`，两者均包含非负 JSON 数字 `input`、`cached_input`、`output`，允许单价为零。模型名与日志中的模型名精确匹配。

每条请求按输入 Token 总数（包含缓存命中 Token）选择价格：小于或等于 `context_threshold` 使用 `short_context`，超过阈值使用 `long_context`。先逐条计算，再按模型及全局合计，费用公式为：

`Price = (Input - Cached input) × input + Cached input × cached_input + Output × output`

缓存写入 Token 使用普通输入价格，已包含在输入 Token 中；思考 Token 已包含在输出 Token 中，两者都不额外累加费用。`service_tier` 标识所选价格表的档位，并显示在查询输出中；当前日志未记录每条请求的服务档位，因此所有选中请求统一按该文件估算。

指定 `--price` 后，以下三张表末尾增加 `Price` 列，底部增加 `Sum` 行，仅合计金额：

- `sum` 的 `Token Usage`：显示每个模型及全部模型的费用。
- `req` 的 `Request Counts`：显示每个模型及全部模型的费用。
- `req` 的 `Request Logs`：显示每条请求的费用。

`Sum` 从请求明细合计，已有的 `All models` 行不会被重复计入。费用以精确十进制计算，仅在显示时四舍五入：小于 `1` 的金额最多保留五位小数，大于或等于 `1` 的金额最多保留三位小数，至少保留两位小数，例如 `USD 0.18915`、`USD 261.165`。`Price` 列显示币种和金额，并左对齐。`--human` 继续格式化请求次数和 Token 数，金额保持十进制显示。

价格表缺少模型，或请求缺少输入、缓存命中、输出 Token 等计费用量时，单条费用显示 `-`。缓存命中 Token 超过输入 Token 等无效数据同样不计价。模型和总计仅累加可计算的请求，存在缺失项时标注 `(partial)`，并在表格后注明未能计价的请求数量；全部无法计价时显示 `- (partial)`。没有匹配请求时合计为零。价格文件无法读取、JSON 无效或必填字段不符合格式时，命令报错。

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

## Codex 模型目录

`GET /v1/models` 使用同一个本地 API Key 认证，代理用配置的 Auth File 请求 `https://chatgpt.com/backend-api/codex/models`。客户端的 `client_version` 等查询参数会原样传递；返回的完整 Codex 模型目录、HTTP 状态码、ETag 和缓存控制头直接转发，不转换为只有模型 ID 的 OpenAI 模型列表，也不在代理内缓存或合并多个账号的目录。账号按 `session-id` 稳定选择，没有该头时使用第一个 Auth File。此接口支持自动刷新令牌和一次 401 重试，不计入 SQLite 推理用量日志。

```bash
curl 'http://127.0.0.1:8317/v1/models?client_version=0.164.0' \
  -H 'Authorization: Bearer sk-local-secret'
```

Codex 使用自定义提供商时，在 `config.toml` 对应提供商下显式配置 `model_catalog_url`，指向代理的目录接口。例如，代理运行在本机：

```toml
model_provider = "codex_pass"

[features]
api_key_model_discovery = true

[model_providers.codex_pass]
name = "Codex Pass"
base_url = "http://127.0.0.1:8317/v1"
wire_api = "responses"
env_key = "CODEX_PASS_KEY"
model_catalog_url = "http://127.0.0.1:8317/v1/models"
```

将 `CODEX_PASS_KEY` 设为代理启动时的 `--key`。远程连接时，把两个 URL 中的 `127.0.0.1:8317` 换成客户端可访问的代理地址。Codex 会自动附加自己的 `client_version`，无需在配置中手写版本号。把 `api_key_model_discovery = true` 加到已有的 `[features]` 表中，不要重复声明该表：桌面端可能通过运行时设置关闭此开关，显式配置可覆盖该设置；关闭时，配置 `model_catalog_url` 会使模型选择器为空。移除生效的 `model_catalog_json` 配置，因为本地目录覆盖会阻止远程获取。配置机制见 [Codex 官方模型目录说明](https://developers.openai.com/codex/model-catalogs)。修改后重启 Codex。

## 上游认证与亲和性

推理请求的上游固定为 `https://chatgpt.com/backend-api/codex/responses`，模型目录请求的上游固定为 `https://chatgpt.com/backend-api/codex/models`。代理替换客户端的 `Authorization` 和 `ChatGPT-Account-ID`，并保留客户端显式提供的 `session-id`、`thread-id`、`originator` 等非认证头。没有显式 `session-id` 时，若推理请求包含字符串 `prompt_cache_key`，代理将该值作为上游 `session-id`。

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
