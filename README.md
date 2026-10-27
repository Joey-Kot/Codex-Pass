# Codex-Pass

English | [简体中文](README_ZH.md)

Codex-Pass is a local proxy for ChatGPT-backed Codex models. It uses your existing Codex authentication files to serve OpenAI-compatible Responses and Chat Completions clients, or to route the official Codex client through its built-in OpenAI provider. It also forwards standalone web search and Codex image generation and editing. It handles upstream token refresh and account affinity, with optional debug logs and SQLite usage reports.

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

### Start the proxy

Copy a Codex authentication file to a dedicated location, then start the proxy:

```bash
./codex-pass --key sk-local-secret --log ./usage /path/to/copied-auth.json
```

The default listen address is `127.0.0.1:8317`. On Windows, use `codex-pass.exe` with the same arguments. All proxy options must appear before the authentication file paths; multiple files are supported.

`--key` and at least one authentication file are required. Official Codex files and supported CPA flattened files are accepted. The proxy refreshes upstream credentials and writes them back to the supplied files, so the files and their parent directories must be writable. Use dedicated copies rather than sharing a file with another process that refreshes it.

### Choose a client route

Both routes can run in the same process:

| Base URL | Clients | Local authentication | Request handling |
| --- | --- | --- | --- |
| `http://127.0.0.1:8317/v1` | OpenAI-compatible clients and Codex custom providers | Bearer token matching `--key` | Responses compatibility adjustments and Chat Completions conversion |
| `http://127.0.0.1:8317/codex` | Official Codex with its built-in OpenAI provider and ChatGPT login | No local Key check; enabled with `--codex` | Original request body forwarded byte for byte |

The `/codex` route is disabled by default. When enabled, it does not validate the client's ChatGPT token; restrict access through the listen address or a front-end proxy. The `/v1` route continues to require its Key. Both routes use the proxy's authentication files for upstream requests.

#### OpenAI-compatible clients: `/v1`

Set the client's Base URL to `http://127.0.0.1:8317/v1` and its API Key to the value passed to `--key`.

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-local-secret' \
  -H 'Content-Type: application/json' \
  --data '{"model":"gpt-6-luna","messages":[{"role":"user","content":"Hello"}]}'
```

The proxy also exposes `POST /v1/responses` with streaming and non-streaming output. `GET /v1/models` returns the full Codex model catalog, including capabilities and instructions; it is not the public OpenAI model-list format.

For a Codex custom provider, add the following to the user-level `~/.codex/config.toml` and make `CODEX_PASS_KEY` available to the Codex process:

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

Merge the feature setting into an existing `[features]` table if present. Explicitly enable `api_key_model_discovery`: the desktop client can disable it through runtime defaults.

#### Codex built-in OpenAI provider: `/codex`

Add `--codex` to the proxy's startup command to enable the dedicated route alongside `/v1`:

```bash
./codex-pass --key sk-local-secret --codex --log ./usage /path/to/copied-auth.json
```

Keep Codex signed in with ChatGPT. Set these top-level values before the first TOML table in the user-level `~/.codex/config.toml`:

```toml
model_provider = "openai"
openai_base_url = "http://127.0.0.1:8317/codex"
```

Codex automatically fetches `/codex/models` and sends inference requests to `/codex/responses`. No custom provider definition, `env_key`, or `model_catalog_url` is needed for this route. Automatic discovery here requires ChatGPT login; API Key clients should use `/v1`.

The dedicated route preserves all request fields, including unknown fields, and supports Codex's zstd compression. An inspection copy supplies log metadata and usage accounting; the original wire bytes are forwarded and reused on retries. WebSocket connection probes switch the client to HTTP/SSE immediately.

For either Codex configuration, remove any effective `model_catalog_json` override so remote discovery can run, then restart Codex. For remote clients, replace `127.0.0.1:8317` with an address reachable from the client. See the official [base URL configuration](https://learn.chatgpt.com/docs/config-file/config-advanced) and [model catalog documentation](https://learn.chatgpt.com/docs/model-catalogs).

### Web search and images

Codex uses separate endpoints for these hosted tools:

| Operation | Custom provider / API Key route | Built-in provider route |
| --- | --- | --- |
| Web search | `POST /v1/alpha/search` | `POST /codex/alpha/search` |
| Image generation | `POST /v1/images/generations` | `POST /codex/images/generations` |
| Image editing | `POST /v1/images/edits` | `POST /codex/images/edits` |

These endpoints forward the Codex JSON request and response bodies unchanged, including reference image data and tool metadata. Image editing accepts Codex's JSON image-input format. The `/v1` routes require the proxy Key; the `/codex` routes require `--codex` to be enabled. For standalone search with a custom provider, advertise `supports_standalone_web_search = true`; the client runtime, model, and configured search mode must also enable the tool.

Tool requests use the same upstream authentication files, refresh/retry behavior, debug logs, and SQLite accounting as inference requests. Returned token usage is recorded when present; missing usage remains unknown. Search and image responses are JSON, so they are not parsed as inference SSE streams.

### Commands and options

```text
codex-pass [options] --key KEY [--codex] AUTH_FILE [AUTH_FILE ...]
codex-pass sum --log DIR [report options]
codex-pass req --log DIR [report options] [--asc | --desc] [--num N]
```

| Option | Scope | Description |
| --- | --- | --- |
| `--key KEY` | Proxy | Required Bearer Key for `/v1`; remains required when `--codex` is enabled. |
| `--listen ADDRESS` | Proxy | Listen address; overrides `CODEX_PROXY_LISTEN`. Defaults to `127.0.0.1:8317`. |
| `--codex` | Proxy | Enable Codex-specific inference, model catalog, search, and image routes. Disabled by default. |
| `--debug FILE` | Proxy | Write detailed JSON Lines debug logs with credential redaction. |
| `--log DIR` | Proxy and reports | Store or query `DIR/codex-pass.sqlite`. Required by `sum` and `req`. |
| `--time RANGE` | Reports | Filter by request start time. Defaults to all recorded requests. |
| `--human` | Reports | Format counts and tokens with compact K/M/B units. |
| `--price FILE` | Reports | Estimate costs using a JSON price table. See [prices.example.json](prices.example.json). |
| `--asc` / `--desc` | `req` | Sort oldest or newest first. Default: newest first. Mutually exclusive. |
| `--num N` | `req` | Limit the report to a positive number of requests after filtering and sorting. |
| `--help` | Any command | Show command-specific help. |

Common options (`--log`, `--time`, `--human`, and `--price`) can appear before or after `sum` or `req`. Request-specific options belong after `req`. Report commands do not start the proxy or require credentials.

```bash
./codex-pass sum --log ./usage --time 1h --human
./codex-pass req --log ./usage --time 1d --num 20
./codex-pass req --log ./usage --asc --num 10 --price ./prices.example.json
./codex-pass req --help
```

Time ranges accept durations such as `2m`, `1h`, `2d`, `1w`, `3M`, and `1y`, or inclusive local date ranges such as `2026-10-01,2026-10-28`. Use `YYYY-MM-DD-HH-MM,YYYY-MM-DD-HH-MM` for minute precision. Lowercase `m` means minutes; uppercase `M` means calendar months.

Usage reports include request outcomes, model names, reasoning effort, and token counts returned by the upstream. Missing usage remains unknown. SQLite stores metadata rather than prompts or credentials; detailed debug logs include request and response content.

### Build from source

Use the Go version specified in [go.mod](go.mod). No CGO or system SQLite library is required.

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o codex-pass ./cmd/codex-pass
```

The [GitHub Actions workflow](.github/workflows/build.yml) tests the project and builds all six target packages on pushes to `main`, pull requests, and manual runs. Successful pushes and manual runs on `main` update the `Latest` release used by the download links above. Pushing a `v*` tag publishes a separate versioned release. Each release includes individual `.sha256` files and a combined `SHA256SUMS` file.

The same packaging script can run locally; use `linux`, `darwin`, or `windows` with `amd64` or `arm64`:

```bash
./scripts/package.sh linux amd64 v1.0.0
```

Linux and macOS packages use `.tar.gz`; Windows packages use `.zip`. Packages include the executable, documentation, MIT license, and example price table. Linux packages also include the systemd examples. The previous detailed README is preserved in [Agents.md](Agents.md).

## Run with systemd

An example unit and environment file are provided in [examples/systemd](examples/systemd). The service runs as a dedicated user, stores credentials and usage data under `/var/lib/codex-pass`, and reads its settings from `/etc/codex-pass/codex-pass.env`.

Run these commands from the repository or an extracted Linux package, replacing the authentication file path:

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

Set `CODEX_PASS_KEY` before starting the service. The example listens on loopback and leaves the Codex-specific route disabled; set `CODEX_PASS_CODEX=true` to enable it. Credentials remain writable for automatic refresh.

```bash
systemctl status codex-pass
journalctl -u codex-pass -f
curl http://127.0.0.1:8317/healthz
```

## License

Released under the [MIT License](LICENSE).
