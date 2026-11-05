# Codex-Pass

English | [简体中文](README_ZH.md)

Codex-Pass is a local proxy that uses ChatGPT credentials to access Codex models. It serves OpenAI-compatible Responses and Chat Completions clients and supports the official Codex client through its built-in OpenAI provider. It forwards standalone web search, image generation, and image editing requests and refreshes credentials automatically. The CLI can check weekly quotas, list and redeem reset credits, inspect account model catalogs, and build usage reports from SQLite logs.

## Downloads

| Platform | Download | SHA-256 |
| --- | --- | --- |
| Linux x86_64 | [linux-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-x86_64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-x86_64.tar.gz.sha256) |
| Linux arm64 | [linux-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-arm64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-linux-arm64.tar.gz.sha256) |
| Windows x86_64 | [windows-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-x86_64.zip) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-x86_64.zip.sha256) |
| Windows arm64 | [windows-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-arm64.zip) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-windows-arm64.zip.sha256) |
| macOS x86_64 | [macos-x86_64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-x86_64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-x86_64.tar.gz.sha256) |
| macOS arm64 | [macos-arm64](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-arm64.tar.gz) | [sha256](https://github.com/Joey-Kot/Codex-Pass/releases/download/Latest/codex-pass-macos-arm64.tar.gz.sha256) |

## Usage

### API routes

Both base URLs can run in the same proxy. Codex-Pass uses its configured auth files for upstream requests on either route.

| Base URL | Use it with | Access |
| --- | --- | --- |
| `http://127.0.0.1:8317/v1` | OpenAI-compatible clients and Codex custom providers | Send one of the local Bearer keys configured with `--key` or `CODEXPASS_KEY`. |
| `http://127.0.0.1:8317/codex` | Codex with its built-in OpenAI provider and ChatGPT login | Enable direct client access with `--codex`. |

#### OpenAI-compatible clients (`/v1`)

Set your client's base URL to `http://127.0.0.1:8317/v1` and its API key to one of the proxy's configured keys.

| Method and path | Purpose |
| --- | --- |
| `POST /v1/responses` | Send Responses API requests with compatibility adjustments for the Codex upstream. Supports streaming SSE and a single JSON response. |
| `POST /v1/chat/completions` | Send Chat Completions requests. The proxy converts `messages` to Responses input and converts the result back to Chat Completions format. |
| `GET /v1/models` | Fetch the upstream Codex model catalog, including model instructions and capabilities. The JSON uses the Codex catalog schema. |

```bash
curl http://127.0.0.1:8317/v1/responses \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","input":"Hello"}'

curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","messages":[{"role":"user","content":"Hello"}]}'
```

Set `stream` to `true` in either inference request to receive SSE events as they arrive.

For a Codex custom provider, set `CODEX_PASS_KEY` in the Codex client's environment to one of the proxy's configured keys. In `~/.codex/config.toml`, place `model_provider` before the first TOML table and merge `api_key_model_discovery` into the existing `[features]` table if present.

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

#### Codex built-in OpenAI provider (`/codex`)

Start the proxy with `--codex` and keep the Codex client signed in with ChatGPT. Put these values before the first TOML table in `~/.codex/config.toml`.

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/codex"
```

| Method and path | Purpose |
| --- | --- |
| `POST /codex/responses` | Forward native Codex inference requests. The proxy preserves the original request bytes and supports zstd compression. WebSocket probes receive HTTP 426 so Codex can switch to HTTP/SSE. |
| `GET /codex/models` | Let the built-in provider discover its model catalog automatically. |

The request body limit is 128 MiB, applied separately to the received and decompressed bodies.

For either Codex setup, remove any active `model_catalog_json` override and restart the client to use remote discovery. For remote clients, use the proxy address reachable by the client. Both model catalog endpoints use `0.166.0` when `client_version` is absent or empty and preserve explicit versions and other query parameters. [Base URL configuration](https://learn.chatgpt.com/docs/config-file/config-advanced) · [Model catalogs](https://learn.chatgpt.com/docs/model-catalogs)

#### Web Search

Use `POST /v1/alpha/search` or `POST /codex/alpha/search` to fetch web search results as a standalone tool call. Send search queries in `commands.search_query`. For automatic calls from Codex, the client and selected model must support standalone search, and the configured search mode must allow it. The custom provider above advertises `supports_standalone_web_search = true`. [Codex search request schema](https://github.com/openai/codex/blob/rust-v0.166.0/codex-rs/codex-api/src/search.rs)

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

#### Image Generation

Use `POST /v1/images/generations` or `POST /codex/images/generations` to create an image from a text prompt. Send a Codex image model and a `prompt` in the JSON body. The upstream can return image data in `data[].b64_json`.

```bash
curl http://127.0.0.1:8317/v1/images/generations \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-image-2","prompt":"A red fox in a snowy forest"}'
```

#### Image Editing

Use `POST /v1/images/edits` or `POST /codex/images/edits` to modify a reference image. Send Codex JSON with the edit instruction in `prompt` and the input image in the `images` array. This example reads `reference.png` from the current directory and sends it as a data URL.

```bash
curl http://127.0.0.1:8317/v1/images/edits \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data-binary @- <<EOF
{
  "model": "gpt-image-2",
  "prompt": "Add a red scarf to the fox",
  "images": [
    {"image_url": "data:image/png;base64,$(base64 < reference.png | tr -d '\r\n')"}
  ]
}
EOF
```

Search, generation, and editing follow the authentication rules for their route prefix. Codex-Pass forwards the Codex JSON bodies as received. When SQLite logging is enabled, it records token counts supplied by the upstream.

#### Health check (`GET /healthz`)

Use an unauthenticated `GET /healthz` for a local liveness probe. The proxy returns HTTP 200 with `{"status":"ok"}` when its HTTP listener is responding.

```bash
curl --fail --silent --show-error http://127.0.0.1:8317/healthz
```

### Start from the command line

Use writable copies of official Codex ChatGPT auth files or supported CPA Codex auth files. Configure at least one local API key with `--key` or `CODEXPASS_KEY`. Put auth file paths after the options when supplying them on the command line. On Windows, use `codex-pass.exe` with the same arguments.

```bash
./codex-pass --key sk-local-secret /path/to/copied-auth.json
```

To enable `/codex` alongside `/v1` and write both logs, use the following command.

```bash
./codex-pass \
  --key sk-local-secret \
  --listen 127.0.0.1:8317 \
  --codex \
  --codexpass-sqlite ./usage \
  --codexpass-debug ./proxy.jsonl \
  /path/to/copied-auth.json
```

The proxy and account commands coordinate token refresh through a sibling lock file with the `.codex-pass.lock` suffix. They atomically update the resolved auth file, so its parent directory must be writable.

### Environment variables

| Variable | Applies to | Purpose |
| --- | --- | --- |
| `CODEXPASS_KEY` | Proxy | One local Bearer key or a comma-separated list of keys. |
| `CODEXPASS_AUTH_FILE` | Proxy, `quota`, `reset`, `models` | Default auth file paths. Separate multiple paths with `:` on Linux and macOS or `;` on Windows. |
| `CODEXPASS_SQLITE` | Proxy, `sum`, `req` | Directory containing `codex-pass.sqlite`. The proxy writes the database and the report commands query it. |
| `CODEXPASS_DEBUG` | Proxy | Path to the JSON Lines debug log. |
| `CODEX_PROXY_LISTEN` | Proxy | Listen address when `--listen` is omitted. Defaults to `127.0.0.1:8317`. |

Command-line paths take precedence. Explicit auth file arguments replace the whole environment list for that invocation. Repeated references to the same file are processed once in first-seen order. In proxy mode, `--codexpass-sqlite=` or `--codexpass-debug=` disables the corresponding log for that run.

`--key` accepts the same comma-separated syntax and replaces the entire `CODEXPASS_KEY` list for that run.

```bash
export CODEXPASS_KEY='sk-local-secret,sk-second-local-secret'
export CODEXPASS_AUTH_FILE='/path/to/first.json:/path/to/second.json'
export CODEXPASS_SQLITE=./usage
export CODEXPASS_DEBUG=./proxy.jsonl
./codex-pass --codex
```

### Run with systemd

Install a build that supports `CODEXPASS_KEY` and create the configuration directory. Linux packages include the service unit and environment template below. [systemd files](examples/systemd)

```bash
sudo install -m 0755 codex-pass /usr/local/bin/codex-pass
sudo install -d -m 0755 /etc/codex-pass
```

Save this unit as `/etc/systemd/system/codex-pass.service`. It runs as `root` to access the files under `/root`.

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

Save the following as `/etc/codex-pass/codex-pass.env` with permissions `0600`. Replace `sk-xxx` with your local API key. The proxy writes refreshed tokens back to `CODEXPASS_AUTH_FILE`.

```ini
CODEX_PROXY_LISTEN="0.0.0.0:8317"
CODEXPASS_SQLITE="/root/codex-log"
CODEXPASS_AUTH_FILE="/root/.codex/auth.json"
CODEXPASS_KEY="sk-xxx"
```

Once the service is running, LAN clients can use `http://<server-address>:8317/codex` as their base URL.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codex-pass
systemctl status codex-pass
journalctl -u codex-pass -f
```

For a service that is already running, use `sudo systemctl restart codex-pass` after reloading the unit to apply the changes.

### Subcommand examples

#### SQLite reports

`sum` aggregates request outcomes and token usage. `req` displays request counts and individual records. Both query an existing SQLite directory supplied with `--codexpass-sqlite` or `CODEXPASS_SQLITE`.

```bash
./codex-pass sum --codexpass-sqlite ./usage --time 1w --human
./codex-pass req --codexpass-sqlite ./usage --time 1d --models gpt-6-astra,gpt-6-luna --num 20
CODEXPASS_SQLITE=./usage ./codex-pass req --asc --num 10 --price ./prices.example.json
```

For `req`, time and model filters apply before sorting and limiting. The request counts, rows, and estimated costs all use the selected records. SQLite stores request metadata and usage. The separate debug log also contains request and response bodies with credential redaction.

#### Account queries and resets

These commands query ChatGPT directly. Pass auth files as arguments or use `CODEXPASS_AUTH_FILE`. The `reset` examples below use the environment variable.

```bash
./codex-pass quota /path/to/copied-auth.json
./codex-pass reset
./codex-pass reset 1
./codex-pass models --client-version 0.166.0 /path/to/copied-auth.json
```

- `quota` lists weekly usage, remaining quota, and the next reset time by auth file.
- `reset` shows the available credit count and a single table with auth file names and expiration times. Numbering and redemption require complete credit details. Credits are numbered from `1` in file order, with the oldest grants first within a file. `reset N` shows the selected credit and asks for `[y/N]` confirmation. Only `y` or `Y` redeems it. Press Enter to cancel.
- `models` shows separate Codex and ChatGPT catalogs for each auth file and includes hidden Codex entries. The ChatGPT catalog identifies models that advertise the image tool. The command lists `gpt-image-2` separately as the built-in image tool's default model. Tool access depends on the account. [Codex image generation](https://learn.chatgpt.com/docs/image-generation)

### Command and option reference

```text
codex-pass [--key KEY[,KEY...]] [--listen ADDRESS] [--codex] [--codexpass-sqlite DIR] [--codexpass-debug FILE] [AUTH_FILE ...]
codex-pass [REPORT_OPTIONS] sum [REPORT_OPTIONS]
codex-pass [REPORT_OPTIONS] req [REPORT_OPTIONS] [--asc | --desc] [--num N] [--models MODEL[,MODEL...]]
codex-pass quota [AUTH_FILE ...]
codex-pass reset [N] [AUTH_FILE ...]
codex-pass models [--client-version VERSION] [AUTH_FILE ...]
codex-pass [COMMAND] --help
```

| Option or argument | Applies to | Usage |
| --- | --- | --- |
| `--key KEY[,KEY...]` | Proxy | Set one or more local Bearer keys and override `CODEXPASS_KEY`. The proxy requires at least one key even when `--codex` is enabled. |
| `--listen ADDRESS` | Proxy | Set the listen address, for example `127.0.0.1:8317`. Overrides `CODEX_PROXY_LISTEN`. |
| `--codex` | Proxy | Enable the `/codex` routes. Off by default. |
| `--codexpass-sqlite DIR` | Proxy, `sum`, `req` | Store or query `DIR/codex-pass.sqlite`. Reports require a directory from this option or `CODEXPASS_SQLITE`. |
| `--codexpass-debug FILE` | Proxy | Write the JSON Lines debug log to `FILE`. Overrides `CODEXPASS_DEBUG`. |
| `--time RANGE` | `sum`, `req` | Filter by request start time. By default, include all recorded times. |
| `--human` | `sum`, `req` | Format counts and token totals with compact K/M/B units. |
| `--price FILE` | `sum`, `req` | Estimate costs using a JSON price table. [Example price table](prices.example.json) |
| `--asc` | `req` | Show oldest requests first. |
| `--desc` | `req` | Show newest requests first. This is the default. |
| `--num N` | `req` | Limit results to a positive number of requests after filtering and sorting. By default, show all matches. |
| `--models MODEL[,MODEL...]` | `req` | Match one or more exact stored model names. |
| `--client-version VERSION` | `models` | Set the version sent to the Codex catalog endpoint. Defaults to `0.166.0`. |
| `--help` | All commands | Show help for the selected command. |
| `AUTH_FILE ...` | Proxy, `quota`, `reset`, `models` | Auth file paths. Supply these after the options, or set `CODEXPASS_AUTH_FILE`. |
| `N` | `reset` | Positive credit number from the current list. Omit it to list credits. |

Use GNU-style long options. Pass values as `--option VALUE` or `--option=VALUE`. Boolean switches also accept `=true` or `=false`. The report options `--codexpass-sqlite`, `--time`, `--human`, and `--price` can appear before or after `sum` or `req`. Put request-specific options after `req`, and choose at most one of `--asc` and `--desc`.

`--time` accepts relative ranges such as `2m`, `1h`, `2d`, `1w`, `3M`, and `1y`. Units are minutes (`m`), hours (`h`), 24-hour days (`d`), 7-day weeks (`w`), calendar months (`M`), and calendar years (`y`). Use `YYYY-MM-DD,YYYY-MM-DD` for local dates or `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM` for local times. The range includes the final date or minute.

## Build from source

Use the Go version declared in `go.mod`. The build uses pure Go for SQLite and compression. [Go module](go.mod)

```bash
git clone https://github.com/Joey-Kot/Codex-Pass.git
cd Codex-Pass
CGO_ENABLED=0 go build -trimpath -o codex-pass ./cmd/codex-pass
```

On Windows, build with `go build -trimpath -o codex-pass.exe ./cmd/codex-pass`. To verify the source, run these checks.

```bash
go test ./...
go test -race ./...
go vet ./...
```

To produce a release archive locally, run the packaging script with a target OS and architecture. Supported targets are `linux`, `darwin`, and `windows` with `amd64` or `arm64`. The GitHub Actions workflow uses the same packaging script. [Release workflow](.github/workflows/build.yml)

```bash
./scripts/package.sh linux amd64 v1.0.0
```

## License

Codex-Pass is licensed under MIT. [Full license text](LICENSE)
