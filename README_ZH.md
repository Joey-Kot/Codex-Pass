# Codex-Pass

[English](README.md) | 简体中文

Codex-Pass 是一个通过 ChatGPT 账号访问 Codex 模型的本地代理。它使用已有的 Codex 认证文件，为兼容 OpenAI 的 Responses 和 Chat Completions 客户端提供服务，也支持官方 Codex 客户端通过内置 OpenAI Provider 接入，并转发独立联网查询、Codex 图片生成和编辑请求。代理负责刷新上游令牌、保持请求与账号的稳定对应关系，并可选记录调试日志和 SQLite 用量统计。

## 下载

| 平台 | 下载 | SHA-256 |
| --- | --- | --- |
| Linux x86_64 | [linux-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-x86_64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-x86_64.tar.gz.sha256) |
| Linux arm64 | [linux-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-arm64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-arm64.tar.gz.sha256) |
| Windows x86_64 | [windows-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-x86_64.zip) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-x86_64.zip.sha256) |
| Windows arm64 | [windows-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-arm64.zip) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-arm64.zip.sha256) |
| macOS x86_64 | [macos-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-x86_64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-x86_64.tar.gz.sha256) |
| macOS arm64 | [macos-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-arm64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-arm64.tar.gz.sha256) |

## 使用方法

### 启动代理

将 Codex 认证文件复制到专用位置，然后启动代理：

```bash
./codex-pass --key sk-local-secret --codexpass-sqlite ./usage /path/to/copied-auth.json
```

默认监听地址为 `127.0.0.1:8317`。Windows 下使用 `codex-pass.exe`，参数相同。所有代理参数都必须放在认证文件路径之前；可以传入多个认证文件。

`--key` 必填，并且至少需要一个认证文件。可以直接传入文件路径，也可以设置 `CODEXPASS_AUTH_FILE`。支持官方 Codex 格式和已支持的 CPA 展平格式。可为每个账户准备一份可写副本，供 Codex-Pass 代理和账户命令共用。刷新通过同目录的 `.codex-pass.lock` 后缀文件协调，令牌会原子写回认证文件实际所在的路径，其父目录需要可写。

### 选择客户端路径

同一个代理进程可以同时提供两种使用方式：

| Base URL | 客户端 | 本地认证 | 请求处理 |
| --- | --- | --- | --- |
| `http://127.0.0.1:8317/v1` | 兼容 OpenAI 的客户端，以及 Codex 自定义 Provider | Bearer Key 必须与 `--key` 一致 | 执行 Responses 兼容调整和 Chat Completions 转换 |
| `http://127.0.0.1:8317/codex` | 使用内置 OpenAI Provider 并通过 ChatGPT 登录的官方 Codex 客户端 | 不校验本地 Key；需要通过 `--codex` 启用 | 按字节转发原始请求体 |

`/codex` 路径默认关闭。启用后，代理不会验证客户端携带的 ChatGPT 令牌，需要通过监听地址或前置代理限制访问。`/v1` 路径仍然要求 Key。两种路径的上游请求都使用代理配置的认证文件。

#### 兼容 OpenAI 的客户端：`/v1`

将客户端的 Base URL 设置为 `http://127.0.0.1:8317/v1`，API Key 设置为启动代理时传给 `--key` 的值。

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","messages":[{"role":"user","content":"Hello"}]}'
```

代理也提供 `POST /v1/responses`，支持流式和非流式输出。`GET /v1/models` 返回包含模型能力和指令的完整 Codex 模型目录，不是公开 OpenAI API 中仅列出模型信息的列表格式。

模型目录请求未提供 `client_version` 或其值为空时，代理自动补为 `0.166.0`；显式传入的版本号和其他查询参数保留原始编码、顺序及重复值。启用后的 `/codex/models` 同样适用。

在 Codex 中使用自定义 Provider 时，将以下配置加入用户级 `~/.codex/config.toml`，并确保 Codex 进程可以读取环境变量 `CODEX_PASS_KEY`：

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
supports_standalone_web_search = true
```

如果已有 `[features]` 表，将开关合并到该表中。请显式启用 `api_key_model_discovery`，因为桌面端可能通过运行时默认设置关闭它。

#### Codex 内置 OpenAI Provider：`/codex`

在代理启动命令中加入 `--codex`，即可在保留 `/v1` 的同时启用专用路径：

```bash
./codex-pass --key sk-local-secret --codex --codexpass-sqlite ./usage /path/to/copied-auth.json
```

保持 Codex 使用 ChatGPT 登录。在用户级 `~/.codex/config.toml` 的第一个 TOML 表之前设置以下顶层配置：

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/codex"
```

Codex 会自动访问 `/codex/models` 获取模型目录，并将推理请求发送到 `/codex/responses`。这条路径无需定义自定义 Provider，也无需配置 `env_key` 或 `model_catalog_url`。此处的自动模型发现依赖 ChatGPT 登录；使用 API Key 的客户端应选择 `/v1`。

专用路径保留全部请求字段，包括未知字段，并支持 Codex 的 zstd 压缩。代理使用检查副本提取日志元数据和用量信息，转发及重试均使用原始请求字节。请求体上限为 128 MiB，接收到的请求体和解压后的请求体分别受此限制。收到 WebSocket 连接探测时，代理会让客户端立即切换到 HTTP/SSE。

上述两种 Codex 配置都需要移除生效的 `model_catalog_json` 覆盖，以便获取远程模型目录，然后重启 Codex。远程客户端应将 `127.0.0.1:8317` 替换为客户端可以访问的地址。配置机制可参考官方的 [Base URL 配置说明](https://learn.chatgpt.com/docs/config-file/config-advanced)和[模型目录说明](https://learn.chatgpt.com/docs/model-catalogs)。

### 联网查询和图片

Codex 为这些内置工具使用独立接口：

| 功能 | 自定义 Provider / API Key 路径 | 内置 Provider 路径 |
| --- | --- | --- |
| 联网查询 | `POST /v1/alpha/search` | `POST /codex/alpha/search` |
| 图片生成 | `POST /v1/images/generations` | `POST /codex/images/generations` |
| 图片编辑 | `POST /v1/images/edits` | `POST /codex/images/edits` |

这些接口原样转发 Codex 的 JSON 请求和响应，包括参考图片数据和工具元数据。图片编辑接受 Codex 使用的 JSON 图片输入格式。`/v1` 路径需要代理 Key，`/codex` 路径需要启用 `--codex`。自定义 Provider 使用独立联网工具时，需要声明 `supports_standalone_web_search = true`，客户端运行时、模型和联网模式也需要启用该工具。

工具请求与推理请求共用上游认证文件、令牌刷新和重试、调试日志及 SQLite 统计。上游返回 Token 用量时会记录实际数值，未返回的字段保持未知。联网和图片响应使用 JSON，不会被当作推理 SSE 流解析。

### 命令和参数

```text
codex-pass [options] --key KEY [--codex] [AUTH_FILE ...]
codex-pass sum [--codexpass-sqlite DIR] [report options]
codex-pass req [--codexpass-sqlite DIR] [report options] [--asc | --desc] [--num N] [--models MODEL[,MODEL...]]
codex-pass quota [AUTH_FILE ...]
codex-pass reset [N] [AUTH_FILE ...]
codex-pass models [--client-version VERSION] [AUTH_FILE ...]
```

| 参数 | 适用范围 | 说明 |
| --- | --- | --- |
| `--key KEY` | 代理 | `/v1` 使用的 Bearer Key，必填；启用 `--codex` 后仍然必填。 |
| `--listen ADDRESS` | 代理 | 监听地址，优先于 `CODEX_PROXY_LISTEN`，默认为 `127.0.0.1:8317`。 |
| `--codex` | 代理 | 启用 Codex 专用的推理、模型目录、联网和图片路径，默认关闭。 |
| `--codexpass-debug FILE` | 代理 | 将脱敏后的 JSON Lines 调试日志写入 `FILE`，默认读取 `CODEXPASS_DEBUG`。 |
| `--codexpass-sqlite DIR` | 代理和报表 | 写入或查询 `DIR/codex-pass.sqlite`，默认读取 `CODEXPASS_SQLITE`。`sum` 和 `req` 必须通过其中一种方式指定目录。 |
| `--time RANGE` | 报表 | 按请求开始时间筛选，默认查询全部记录。 |
| `--human` | 报表 | 使用 K/M/B 单位简化请求次数和 Token 数的显示。 |
| `--price FILE` | 报表 | 根据 JSON 价格表估算费用，参见 [prices.example.json](prices.example.json)。 |
| `--asc` / `--desc` | `req` | 按从旧到新或从新到旧排序，默认从新到旧；两个参数互斥。 |
| `--num N` | `req` | 筛选和排序后，最多显示指定数量的请求；必须为正整数。 |
| `--models MODEL[,MODEL...]` | `req` | 精确匹配一个或多个已记录的模型名，先筛选再排序和应用 `--num`。 |
| `--help` | 所有命令 | 显示对应命令的帮助。 |

所有选项均使用 `--选项名`。命令行传入的路径优先于环境变量。公共参数 `--codexpass-sqlite`、`--time`、`--human` 和 `--price` 可以放在 `sum` 或 `req` 前后。请求明细专用参数需要放在 `req` 后面。报表命令不会启动代理，也不需要凭据。

```bash
./codex-pass sum --codexpass-sqlite ./usage --time 1h --human
./codex-pass req --codexpass-sqlite ./usage --time 1d --num 20
./codex-pass req --codexpass-sqlite ./usage --asc --num 10 --price ./prices.example.json
CODEXPASS_SQLITE=./usage ./codex-pass req --models gpt-6-astra,gpt-6-luna --num 20
./codex-pass req --help
```

时间范围支持 `2m`、`1h`、`2d`、`1w`、`3M`、`1y` 等时长，也支持 `2026-10-01,2026-10-28` 这样的本地日期范围，包含起止日期。需要精确到分钟时，使用 `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM`。小写 `m` 表示分钟，大写 `M` 表示日历月。

用量报表包含请求结果、模型名称、思考强度，以及上游实际返回的 Token 数。`req` 的明细、请求次数和费用按同一批筛选结果计算。缺失的用量保持未知。SQLite 只保存元数据，不保存提示词或凭据；详细调试日志则包含请求和响应正文。

### 账户周配额、重置券和模型

账户命令和代理都可以从环境变量 `CODEXPASS_AUTH_FILE` 读取一个或多个 Codex 或 CPA 认证文件。Linux 和 macOS 用冒号 `:` 分隔路径，Windows 用分号 `;`。命令行传入文件路径时，本次只使用这些路径。同一文件只处理一次，以首次传入的路径和顺序为准。账户命令直接访问 ChatGPT，并沿用代理的令牌刷新机制。

```bash
export CODEXPASS_AUTH_FILE='/path/to/first.json:/path/with spaces/second.json'
./codex-pass quota
./codex-pass reset
./codex-pass reset 2
./codex-pass models /path/to/another.json /path/to/fourth.json
```

- `quota` 按认证文件分组，显示各个周配额窗口的已用比例、剩余比例和下次重置时间。
- `reset` 显示可用重置券总数，并用一张表列出券号、认证文件名和到期时间。所有文件的券详情完整后才可编号和兑换。券号从 `1` 开始，按文件传入顺序排列，同一文件内按领取时间从早到晚。`reset N` 重新读取当前列表，显示所选券和所属文件，再提示 `[y/N]`。只有输入 `y` 或 `Y` 才会兑换，直接回车、输入无效或标准输入关闭都会取消。时间使用运行机器的本地时区。
- `models` 按认证文件分组，分别显示各账户的 Codex 和 ChatGPT 模型目录，保留 Codex 选择器隐藏的条目。可通过 `--client-version VERSION` 指定 Codex 目录版本。ChatGPT 目录会标注哪些模型提供图像工具。单列的 `gpt-image-2` 是 Codex 内置图像工具的默认模型，工具权限取决于账户套餐和工作区设置。 [OpenAI 图像生成文档](https://learn.chatgpt.com/docs/image-generation)

### 从源码构建

使用 [go.mod](go.mod) 指定的 Go 版本，无需 CGO 或系统 SQLite 库。

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o codex-pass ./cmd/codex-pass
```

[GitHub Actions 工作流](.github/workflows/build.yml)会在推送到 `main`、创建拉取请求或手动运行时测试项目并构建六个目标包。`main` 上成功完成的推送构建和手动构建会更新下载链接所指向的 `Latest` Release。推送 `v*` 标签会发布独立的版本 Release。每次发布都包含各个包的 `.sha256` 文件，以及汇总校验文件 `SHA256SUMS`。

也可以在本地运行相同的打包脚本，操作系统使用 `linux`、`darwin` 或 `windows`，架构使用 `amd64` 或 `arm64`：

```bash
./scripts/package.sh linux amd64 v1.0.0
```

Linux 和 macOS 使用 `.tar.gz` 压缩包，Windows 使用 `.zip`。压缩包包含可执行文件、文档、MIT 许可证和示例价格表；Linux 压缩包还包含 systemd 示例。原来的详细 README 完整保留在 [Agents.md](Agents.md) 中。

## 使用 systemd

[examples/systemd](examples/systemd) 中提供了 unit 和环境配置文件示例。服务使用独立用户运行，将凭据和用量数据保存在 `/var/lib/codex-pass`，并从 `/etc/codex-pass/codex-pass.env` 读取设置。

在仓库目录或解压后的 Linux 安装包目录中执行以下命令，并替换认证文件路径：

```bash
sudo install -m 0755 codex-pass /usr/local/bin/codex-pass
sudo useradd --system --home-dir /var/lib/codex-pass --shell /usr/sbin/nologin codex-pass
sudo install -d -m 0755 /etc/codex-pass
sudo install -d -m 0700 -o codex-pass -g codex-pass /var/lib/codex-pass
sudo install -m 0600 -o codex-pass -g codex-pass /path/to/copied-auth.json /var/lib/codex-pass/auth.json
sudo install -m 0600 examples/systemd/codex-pass.env.example /etc/codex-pass/codex-pass.env
sudoedit /etc/codex-pass/codex-pass.env
sudo install -m 0644 examples/systemd/codex-pass.service /etc/systemd/system/codex-pass.service
sudo systemctl daemon-reload
sudo systemctl enable --now codex-pass
```

启动服务前设置 `CODEX_PASS_KEY`。示例默认监听回环地址，并关闭 Codex 专用路径；将 `CODEX_PASS_CODEX` 设置为 `true` 即可启用。认证文件保持可写，以便自动刷新凭据。

```bash
systemctl status codex-pass
journalctl -u codex-pass -f
curl http://127.0.0.1:8317/healthz
```

## 许可证

本项目使用 [MIT 许可证](LICENSE)。
