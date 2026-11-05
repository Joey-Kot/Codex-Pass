# Codex-Pass

[English](README.md) | 简体中文

Codex-Pass 是一个使用 ChatGPT 账户凭据访问 Codex 模型的本地代理。它为兼容 OpenAI API 的客户端提供 Responses 和 Chat Completions 接口，也支持官方 Codex 客户端通过内置 OpenAI Provider 接入。代理可转发独立联网搜索、图像生成和图像编辑请求，并自动刷新令牌。通过命令行可以查询各账户的周配额和模型目录、查看或兑换重置券，并从 SQLite 请求日志生成用量报表。

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

### API 路由

两个入口可以在同一个代理进程中使用。Codex-Pass 会用代理配置的认证文件向上游发起请求。

| Base URL | 适用客户端 | 访问方式 |
| --- | --- | --- |
| `http://127.0.0.1:8317/v1` | 兼容 OpenAI API 的客户端、Codex 自定义 Provider | 发送通过 `--key` 或 `CODEXPASS_KEY` 配置的任意一个本地 Bearer Key。 |
| `http://127.0.0.1:8317/codex` | 使用内置 OpenAI Provider 并通过 ChatGPT 登录的 Codex | 通过 `--codex` 启用，客户端可直接访问。 |

#### OpenAI 兼容入口（`/v1`）

将客户端的 Base URL 设为 `http://127.0.0.1:8317/v1`，API Key 填写代理已配置的任意一个 Key。

| 方法和路径 | 用途 |
| --- | --- |
| `POST /v1/responses` | 提交 Responses API 请求。代理对请求做 Codex 上游兼容处理，支持 SSE 流式输出和单个 JSON 响应。 |
| `POST /v1/chat/completions` | 提交 Chat Completions 请求。代理将 `messages` 转成 Responses 输入，再把结果转换为 Chat Completions 格式。 |
| `GET /v1/models` | 获取上游 Codex 模型目录，包含模型指令和能力配置。JSON 使用 Codex 模型目录格式。 |

```bash
curl http://127.0.0.1:8317/v1/responses \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","input":"你好"}'

curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","messages":[{"role":"user","content":"你好"}]}'
```

两个推理接口都可以通过 `stream: true` 接收实时 SSE 事件。

用 Codex 自定义 Provider 接入时，将客户端的环境变量 `CODEX_PASS_KEY` 设为代理已配置的任意一个 Key。在 `~/.codex/config.toml` 中，将 `model_provider` 放在第一个 TOML 表之前。如果已有 `[features]` 表，把 `api_key_model_discovery` 合并进去。

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

#### Codex 内置 OpenAI Provider 入口（`/codex`）

用 `--codex` 启动代理，Codex 客户端保持 ChatGPT 登录。在 `~/.codex/config.toml` 的第一个 TOML 表之前加入以下设置。

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/codex"
```

| 方法和路径 | 用途 |
| --- | --- |
| `POST /codex/responses` | 转发 Codex 原生推理请求。代理保留原始请求字节，支持 zstd 压缩。WebSocket 探测会收到 HTTP 426，Codex 据此切换到 HTTP/SSE。 |
| `GET /codex/models` | 供内置 Provider 自动发现模型目录。 |

请求体上限为 128 MiB，接收到的请求体和解压后的请求体分别受此限制。

两种 Codex 配置都需移除生效的 `model_catalog_json` 覆盖并重启客户端，才能获取远程目录。客户端在另一台机器上运行时，改用它能访问的代理地址。两个模型目录接口在 `client_version` 缺失或为空时使用 `0.166.0`，并保留显式传入的版本和其他查询参数。 [Base URL 配置](https://learn.chatgpt.com/docs/config-file/config-advanced) · [模型目录说明](https://learn.chatgpt.com/docs/model-catalogs)

#### Web Search（联网搜索）

通过 `POST /v1/alpha/search` 或 `POST /codex/alpha/search` 单独执行联网搜索。查询放在 JSON 的 `commands.search_query` 中。由 Codex 自动调用时，客户端和所选模型都需支持独立联网搜索，搜索模式也需允许调用。上面的自定义 Provider 已声明 `supports_standalone_web_search = true`。 [Codex 搜索请求格式](https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/codex-api/src/search.rs)

```bash
curl http://127.0.0.1:8317/v1/alpha/search \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data-binary @- <<'JSON'
{
  "id": "d95493fa-2a91-4e1e-a1b1-934bca0fb441",
  "model": "gpt-6.1-sol",
  "commands": {"search_query": [{"q": "Go context package", "domains": ["go.dev"]}]},
  "settings": {"external_web_access": true}
}
JSON
```

#### Image Generation（图像生成）

通过 `POST /v1/images/generations` 或 `POST /codex/images/generations` 根据文字生成图片。在 JSON 中指定图像模型和 `prompt`。上游返回的图片数据可从 `data[].b64_json` 读取。

```bash
curl http://127.0.0.1:8317/v1/images/generations \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-image-2","prompt":"雪林中的一只红狐"}'
```

#### Image Editing（图像编辑）

通过 `POST /v1/images/edits` 或 `POST /codex/images/edits` 修改参考图。在 Codex JSON 请求中，将编辑要求放入 `prompt`，参考图放入 `images` 数组。下面的命令会读取当前目录中的 `reference.png` 并将它作为数据 URL 提交。

```bash
curl http://127.0.0.1:8317/v1/images/edits \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data-binary @- <<EOF
{
  "model": "gpt-image-2",
  "prompt": "给狐狸加一条红色围巾",
  "images": [
    {"image_url": "data:image/png;base64,$(base64 < reference.png | tr -d '\r\n')"}
  ]
}
EOF
```

联网搜索、图像生成和图像编辑遵循各自路由前缀的认证规则。代理原样转发 Codex JSON 请求和响应。启用 SQLite 日志后，代理会记录上游返回的 Token 用量。

#### 健康检查（`GET /healthz`）

用无需认证的 `GET /healthz` 做存活探测。本地 HTTP 服务正常响应时返回 HTTP 200 和 `{"status":"ok"}`。

```bash
curl --fail --silent --show-error http://127.0.0.1:8317/healthz
```

### 命令行启动

准备可写的官方 Codex ChatGPT 认证文件副本，或使用已支持的 CPA Codex 认证文件。通过 `--key` 或 `CODEXPASS_KEY` 至少设置一个本地 API Key。通过命令行指定认证文件时，把路径放在所有选项之后。Windows 下使用 `codex-pass.exe`，参数相同。

```bash
./codex-pass --key sk-local-secret /path/to/copied-auth.json
```

下面的命令同时开放 `/v1` 和 `/codex`，并开启 SQLite 用量日志和调试日志。

```bash
./codex-pass \
  --key sk-local-secret \
  --listen 127.0.0.1:8317 \
  --codex \
  --codexpass-sqlite ./usage \
  --codexpass-debug ./proxy.jsonl \
  /path/to/copied-auth.json
```

代理和账户命令通过认证文件旁带有 `.codex-pass.lock` 后缀的文件协调令牌刷新。令牌会原子写回认证文件实际所在的路径，因此其父目录需要可写。

### 环境变量

| 变量 | 适用范围 | 用途 |
| --- | --- | --- |
| `CODEXPASS_KEY` | 代理 | 本地 Bearer Key，可填写一个或用逗号分隔的多个。 |
| `CODEXPASS_AUTH_FILE` | 代理、`quota`、`reset`、`models` | 默认认证文件路径。多个路径在 Linux 和 macOS 下用 `:` 分隔，在 Windows 下用 `;` 分隔。 |
| `CODEXPASS_SQLITE` | 代理、`sum`、`req` | `codex-pass.sqlite` 所在目录。代理写入数据库，报表子命令查询数据库。 |
| `CODEXPASS_DEBUG` | 代理 | JSON Lines 调试日志文件路径。 |
| `CODEX_PROXY_LISTEN` | 代理 | 未指定 `--listen` 时使用的监听地址，默认是 `127.0.0.1:8317`。 |

命令行路径优先。显式传入认证文件时，本次使用这些文件替代环境变量中的整组路径。同一文件按首次出现的位置处理一次。代理模式下，可用 `--codexpass-sqlite=` 或 `--codexpass-debug=` 关闭本次启动对应的日志。

`--key` 同样支持用逗号分隔多个 Key。显式传入时，本次启动使用命令行指定的整组 Key。

```bash
export CODEXPASS_KEY='sk-local-secret,sk-second-local-secret'
export CODEXPASS_AUTH_FILE='/path/to/first.json:/path/to/second.json'
export CODEXPASS_SQLITE=./usage
export CODEXPASS_DEBUG=./proxy.jsonl
./codex-pass --codex
```

### 使用 systemd

安装支持 `CODEXPASS_KEY` 的新版程序，并创建配置目录。Linux 发行包已包含下方的服务文件和环境变量模板。 [systemd 示例文件](examples/systemd)

```bash
sudo install -m 0755 codex-pass /usr/local/bin/codex-pass
sudo install -d -m 0755 /etc/codex-pass
```

将下面的 unit 保存为 `/etc/systemd/system/codex-pass.service`。服务以 `root` 运行，直接读取 `/root` 下的文件。

```ini
[Unit]
Description=Codex Pass Service
After=network.target

[Service]
Type=simple
User=root
Restart=always
RestartSec=3s
EnvironmentFile=/etc/codex-pass/codex-pass.env
ExecStart=/usr/local/bin/codex-pass --codex

[Install]
WantedBy=multi-user.target
```

将下面的内容保存为 `/etc/codex-pass/codex-pass.env`，文件权限设为 `0600`。把 `sk-xxx` 换成你的本地 API Key。代理刷新令牌后会写回 `CODEXPASS_AUTH_FILE` 指向的文件。

```ini
CODEX_PROXY_LISTEN="0.0.0.0:8317"
CODEXPASS_SQLITE="/root/codex-log"
CODEXPASS_AUTH_FILE="/root/.codex/auth.json"
CODEXPASS_KEY="sk-xxx"
```

服务启动后，局域网客户端可将 Base URL 设为 `http://<服务器地址>:8317/codex`。

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codex-pass
systemctl status codex-pass
journalctl -u codex-pass -f
```

如果服务已在运行，执行 `daemon-reload` 后用 `sudo systemctl restart codex-pass` 应用新配置。

### 子命令用法示例

#### SQLite 用量报表

`sum` 汇总请求结果和 Token 用量，`req` 显示请求次数和逐条记录。两者都通过 `--codexpass-sqlite` 或 `CODEXPASS_SQLITE` 查询已有的 SQLite 数据库。

```bash
./codex-pass sum --codexpass-sqlite ./usage --time 1w --human
./codex-pass req --codexpass-sqlite ./usage --time 1d --models gpt-6-astra,gpt-6-luna --num 20
CODEXPASS_SQLITE=./usage ./codex-pass req --asc --num 10 --price ./prices.example.json
```

`req` 先按时间和模型筛选，再排序和限制条数。请求次数、明细和估算费用使用同一批记录。SQLite 保存请求元数据和用量，单独的调试日志还会保存请求与响应正文，并对凭据脱敏。

#### 账户查询与重置

账户子命令直接查询 ChatGPT。可以传入认证文件路径，也可以使用 `CODEXPASS_AUTH_FILE`。下面的 `reset` 示例使用环境变量中的文件。

```bash
./codex-pass quota /path/to/copied-auth.json
./codex-pass reset
./codex-pass reset 1
./codex-pass models --client-version 0.166.0 /path/to/copied-auth.json
```

- `quota` 按认证文件显示周配额已用比例、剩余比例和下次重置时间。
- `reset` 显示可用券总数，并用一张表列出各张券的所属文件和到期时间。所有文件的券详情完整后才会编号并允许兑换。券号从 `1` 开始，按文件顺序排列，同一文件内先列领取较早的券。运行 `reset N` 后，程序会显示所选券并提示 `[y/N]`。输入 `y` 或 `Y` 才会兑换，直接回车则取消。
- `models` 按认证文件分别显示 Codex 和 ChatGPT 模型目录，保留 Codex 选择器隐藏的条目。ChatGPT 目录标注哪些模型提供图像工具。Codex 内置图像工具的默认模型 `gpt-image-2` 单独列出，能否使用取决于账户权限。 [Codex 图像生成](https://learn.chatgpt.com/docs/image-generation)

### 完整命令和参数

```text
codex-pass [--key KEY[,KEY...]] [--listen ADDRESS] [--codex] [--codexpass-sqlite DIR] [--codexpass-debug FILE] [AUTH_FILE ...]
codex-pass [REPORT_OPTIONS] sum [REPORT_OPTIONS]
codex-pass [REPORT_OPTIONS] req [REPORT_OPTIONS] [--asc | --desc] [--num N] [--models MODEL[,MODEL...]]
codex-pass quota [AUTH_FILE ...]
codex-pass reset [N] [AUTH_FILE ...]
codex-pass models [--client-version VERSION] [AUTH_FILE ...]
codex-pass [COMMAND] --help
```

| 参数 | 适用范围 | 用法 |
| --- | --- | --- |
| `--key KEY[,KEY...]` | 代理 | 设置一个或多个本地 Bearer Key，覆盖 `CODEXPASS_KEY`。启用 `--codex` 时也需至少配置一个 Key。 |
| `--listen ADDRESS` | 代理 | 设置监听地址，例如 `127.0.0.1:8317`。优先于 `CODEX_PROXY_LISTEN`。 |
| `--codex` | 代理 | 启用 `/codex` 路由，默认关闭。 |
| `--codexpass-sqlite DIR` | 代理、`sum`、`req` | 写入或查询 `DIR/codex-pass.sqlite`。报表命令需通过此参数或 `CODEXPASS_SQLITE` 指定目录。 |
| `--codexpass-debug FILE` | 代理 | 将 JSON Lines 调试日志写入 `FILE`，优先于 `CODEXPASS_DEBUG`。 |
| `--time RANGE` | `sum`、`req` | 按请求开始时间筛选。默认查询全部时间。 |
| `--human` | `sum`、`req` | 用 K/M/B 等紧凑单位显示请求次数和 Token 数。 |
| `--price FILE` | `sum`、`req` | 使用 JSON 价格表估算费用。 [示例价格表](prices.example.json) |
| `--asc` | `req` | 按请求开始时间从旧到新排列。 |
| `--desc` | `req` | 按请求开始时间从新到旧排列，这是默认顺序。 |
| `--num N` | `req` | 筛选和排序后最多显示 `N` 条，要求正整数。默认显示全部匹配记录。 |
| `--models MODEL[,MODEL...]` | `req` | 精确匹配数据库中的模型名，多个名称用逗号分隔。 |
| `--client-version VERSION` | `models` | 指定查询 Codex 模型目录时发送的客户端版本，默认是 `0.166.0`。 |
| `--help` | 所有命令 | 显示对应命令的帮助。 |
| `AUTH_FILE ...` | 代理、`quota`、`reset`、`models` | 认证文件路径，放在选项之后。也可以设置 `CODEXPASS_AUTH_FILE`。 |
| `N` | `reset` | 要兑换的券号，必须为正整数。省略时列出可用券。 |

选项统一使用 GNU 风格的长选项。选项值可写成 `--选项名 值` 或 `--选项名=值`，开关也可显式传入 `=true` 或 `=false`。报表公共选项 `--codexpass-sqlite`、`--time`、`--human` 和 `--price` 可以放在 `sum` 或 `req` 前后。`req` 的专用选项放在子命令后，`--asc` 和 `--desc` 二选一。

`--time` 支持 `2m`、`1h`、`2d`、`1w`、`3M` 和 `1y` 等相对时长。单位依次是分钟、小时、24 小时、7 天、日历月和日历年。可用 `YYYY-MM-DD,YYYY-MM-DD` 指定本地日期范围，或用 `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM` 精确到分钟，范围包含结束日期或结束分钟。

## 从源码构建

使用 `go.mod` 指定的 Go 版本。SQLite 和压缩功能均使用纯 Go 实现。 [Go 模块配置](go.mod)

```bash
git clone https://github.com/Joey-Kot/Codex-Pass.git
cd Codex-Pass
CGO_ENABLED=0 go build -trimpath -o codex-pass ./cmd/codex-pass
```

Windows 下执行 `go build -trimpath -o codex-pass.exe ./cmd/codex-pass`。也可以运行以下检查验证源码。

```bash
go test ./...
go test -race ./...
go vet ./...
```

生成发行压缩包时，指定目标操作系统和架构。系统支持 `linux`、`darwin` 和 `windows`，架构支持 `amd64` 和 `arm64`。GitHub Actions 使用同一个打包脚本。 [发布工作流](.github/workflows/build.yml)

```bash
./scripts/package.sh linux amd64 v1.0.0
```

## 许可证

本项目采用 MIT 许可证。 [许可证全文](LICENSE)
