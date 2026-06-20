# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Kiro-Go is a reverse proxy service that translates Kiro API requests into OpenAI and Anthropic (Claude) compatible formats. It enables multi-account pooling with round-robin load balancing, automatic OAuth token refresh, real-time streaming, and a web-based admin panel for account and configuration management.

**Core Features:**
- Anthropic `/v1/messages` & OpenAI `/v1/chat/completions` endpoints
- Multi-account pool with priority-based selection (highest weight first)
- Auto token refresh and SSE streaming support
- Multiple auth methods: AWS Builder ID, IAM Identity Center (Enterprise SSO), SSO Token, local cache, credentials JSON
- Usage tracking and pricing calibration
- Web admin panel (built-in, no separate frontend repo) with request logs viewer
- Support for outbound proxies (SOCKS5 / HTTP)
- Regional data-plane routing (profile ARN region auto-detection)

## Quick Build & Test Commands

**Build:**
```bash
go build -o kiro-go .
```

**Run:**
```bash
# Default: reads config from data/config.json
./kiro-go

# Custom config path
CONFIG_PATH=/path/to/config.json ./kiro-go

# Override admin password via env var
ADMIN_PASSWORD=secure_pass ./kiro-go
```

**Tests:**
```bash
# Run all tests
go test ./...

# Run tests in a specific package
go test ./proxy

# Run a specific test (verbose)
go test -v ./proxy -run TestThinkingSourceReasoningFirst

# Run tests with coverage
go test -cover ./...
```

**Docker:**
```bash
# Build Docker image (multi-platform: amd64, arm64)
docker build -t kiro-go .

# Run with docker-compose
docker-compose up -d

# Run standalone container
docker run -d -p 8080:8080 -e ADMIN_PASSWORD=pass -v ./data:/app/data kiro-go
```

**Logging:**
```bash
# Set log level via env var (default: "info")
# Options: "debug", "info", "warn", "error"
LOG_LEVEL=debug ./kiro-go
```

## Architecture

### High-Level Flow

```
HTTP Request
    ↓
proxy/handler.go (HTTP Handler)
    ↓
Request Routing: /v1/messages (Claude) or /v1/chat/completions (OpenAI)
    ↓
proxy/translator.go (Convert request format)
    ↓
pool/account.go (Select highest-priority available account)
    ↓
auth/*.go (Refresh token if needed)
    ↓
proxy/kiro_api.go (Regionalize endpoint URL based on profile ARN)
    ↓
proxy/kiro.go (Call upstream Kiro API with streaming)
    ↓
proxy/responses_handler.go (Parse Event Stream response)
    ↓
proxy/translator.go (Convert response to target format)
    ↓
HTTP Response (stream or complete)
```

### Key Packages

**`config/`** - Persistent configuration management
- `config.go`: Account storage, settings (port, host, API keys, passwords), usage metrics, thinking mode config
- `apikeys.go`: API key management (for proxy authentication)
- `state.go`: Runtime state (request counters, per-account stats) split from config; persisted to `data/config.state.json` with atomic writes
- Thread-safe read-write mutex protection around JSON-persisted state
- Single source of truth: `data/config.json` (or `CONFIG_PATH`)

**`auth/`** - Authentication & token refresh
- `builderid.go`: AWS Builder ID OIDC device flow login
- `iam_sso.go`: AWS IAM Identity Center (Enterprise SSO) integration
- `oidc.go`: Generic OIDC token refresh (IdC/Builder ID)
- `sso_token.go`: Social/GitHub token refresh
- `http_client.go`: HTTP client with outbound proxy support
- All methods return: `(accessToken, refreshToken, expiresAt, profileArn, error)`

**`pool/`** - Account pool & load balancing
- `account.go`: Priority-based selection (accounts sorted by weight descending, first available wins), cooldown on errors, per-account fail tracking
- Accounts marked over-quota are skipped unless `AllowOverUsage` is enabled or per-account `OverageStatus=ENABLED`
- Token refresh skew: 120 seconds before expiration to proactively refresh
- Filters disabled accounts and quota-blocked accounts; falls back to shortest-cooldown account when all are cooling down

**`proxy/`** - Core request/response translation & streaming
- `handler.go` (~3800 lines): Main HTTP handler, routes to `/v1/messages`, `/v1/chat/completions`, and `/v1/responses`; manages background refresh task; request logs (in-memory ring buffer, 500 entries)
- `translator.go` (~2450 lines): Bidirectional translation between Kiro API ↔ Claude/OpenAI formats, token estimation, thinking mode handling
- `kiro.go` (~960 lines): Kiro API endpoint selection, EventStream parsing, streaming response handling
- `kiro_api.go`: REST API helpers (GetUsageLimits, GetUserInfo, ListAvailableModels, ResolveProfileArn); regional URL routing (`regionalizeURL` / `regionalizeURLForProfile`); BuilderId profile lookup suppression (24h cooldown)
- `cache_tracker.go`: Tracks prompt cache usage (Claude's @-tagging feature)
- `responses_handler.go` / `responses_types.go` / `responses_input.go` / `responses_history.go` / `responses_store.go`: OpenAI Responses API (`/v1/responses`) implementation — stateful multi-turn via `previous_response_id`, stored responses persisted to `data/responses/` with 30-day TTL auto-purge
- `account_failover.go`: Per-request account retry logic (up to 3 attempts); classifies upstream errors as quota, overage, suspension, or profile-unavailable to decide cooldown vs. skip; sanitizes internal errors before reaching clients
- `auth.go`: API key authentication middleware; injects the matched `ApiKeyEntry` into request context
- `kiro_headers.go`: Builds Kiro-specific HTTP headers (`x-amzn-codewhisperer-*`, User-Agent) from account state
- `kiro_overage.go`: Calls the AWS Q API to read and toggle per-account Overages switch (`setUserPreference`)
- `stop_sequence_filter.go`: Adapter-side stop_sequences enforcement (upstream doesn't support stop param); scans visible text only (thinking exempt)
- `tool_leak_filter.go`: Rescues tool-call XML leaked into assistant text; cross-frame filtering with config.json toggle
- `token_estimator.go`: Rough token estimation for request bodies (used when exact counts are unavailable)
- `tokenizer.go`: tiktoken BPE tokenizer integration for accurate output token counting
- `pricing.go` / `pricing_updater.go`: Model pricing calibration and auto-update from upstream; runtime cache at `data/model_pricing.json`, fallback bundled at `proxy/model_pricing.json`

**`logger/`** - Structured logging
- `logger.go`: Simple logger with level control (debug, info, warn, error)

**`web/`** - Admin panel frontend (browser-based)
- Static HTML/CSS/JS files served from HTTP handler
- No separate frontend build step; files are embedded or served directly
- `styles.css`: Standalone stylesheet (3600+ lines)
- Tabs: Accounts, Settings, API, Filter (prompt injection/regex), Logs (request history)
- Localization: `web/locales/en.json` and `web/locales/zh.json`

### Config Structure

The `config.json` file stored in `CONFIG_PATH` (default: `data/config.json`) contains:

```json
{
  "port": 8080,
  "host": "0.0.0.0",
  "machineId": "uuid",
  "apiKey": "proxy_key",
  "requireApiKey": true,
  "adminPassword": "hash",
  "accounts": [
    {
      "id": "uuid",
      "email": "user@example.com",
      "userId": "kiro_user_id",
      "nickname": "My Account",
      "accessToken": "token",
      "refreshToken": "token",
      "authMethod": "idc" | "social",
      "clientId": "...",
      "clientSecret": "...",
      "region": "us-east-1",
      "profileArn": "arn:aws:codewhisperer:{region}:{accountId}:profile/{name}",
      "provider": "BuilderId" | "IAMIdentityCenter",
      "weight": 1,
      "enabled": true,
      "usageCurrent": 0.0,
      "usageLimit": 100.0,
      "overageStatus": "ENABLED" | "DISABLED" | "UNKNOWN",
      ...
    }
  ],
  "settings": {
    "thinkingMode": "enabled" | "disabled",
    "outboundProxyUrl": "socks5://...",
    "allowOverUsage": false
  }
}
```

### Request/Response Translation

**Inbound:** Kiro-Go accepts both Claude (`/v1/messages`) and OpenAI (`/v1/chat/completions`) formats, normalizes to a common internal format, then translates to Kiro's `generateAssistantResponse` API call.

**Outbound:** Responses from Kiro are parsed from AWS Event Stream format (binary protocol with headers/payload chunks) into a structured response, then translated back to the requested format (Claude or OpenAI).

**Key Conversions:**
- Token counting is calibrated against model pricing data (pulled from upstream)
- Thinking mode: Claude requests with `thinking` config are transformed to internal thinking markers; thinking blocks are extracted from responses
- Tool calls: Format differs between Claude and OpenAI; translator converts between both
- Images: Handled as base64 or URLs; normalized for Kiro API

### Streaming & SSE

- Responses use Server-Sent Events (SSE) when the client requests streaming (`stream: true`)
- Handler pipes chunks directly from Kiro's EventStream parser to the response writer
- `WriteTimeout` is intentionally set to 0 to allow SSE streams to run for minutes
- `ReadHeaderTimeout` (30s) and `ReadTimeout` (60s) prevent slowloris attacks

### Token Refresh Lifecycle

1. When a request arrives, handler checks if account token is within 120 seconds of expiration
2. If expired or near-expiration, triggers async refresh via `auth.RefreshToken()`
3. Refresh calls the appropriate OIDC or social endpoint (depends on `authMethod`)
4. On success, account state is updated in config and persisted
5. If refresh fails, account enters cooldown (configurable, default ~5 minutes)
6. Requests route to next available account; the cooled-down account is retried after cooldown

### Deployment

**Docker:** Multi-stage build (Go builder on native platform, final image on Alpine). Web files are copied in. Exposed on port 8080.

**Systemd:** See `deploy/README.md` for multi-instance setup using systemd template units (`kiro-go@.service`). Each instance has:
- Isolated `instances/<name>/config.json` (set via `CONFIG_PATH`)
- Separate port configured in that config
- Dedicated log file `/var/log/kiro-go/<name>.log` with hourly rotation
- Shared binary (`kiro-go`)

**CI/CD:** GitHub Actions workflow (`.github/workflows/docker.yml`) builds and pushes Docker images to GHCR on push/PR/tag.

## Test Infrastructure

29 test files across the codebase using Go's standard `testing` package. Tests often:
- Create temp config files (`t.TempDir()`) for isolated testing
- Mock HTTP endpoints using `httptest.Server`
- Use `config.Init()` to bootstrap test configs
- Mock test hooks (in `auth/testhooks.go`) to intercept auth flows

Key test areas:
- `config_test.go`: Settings updates, API key management
- `handler_test.go`: HTTP request/response, streaming, model listing
- `translator_test.go`: Request/response translation, token counting, thinking mode
- `pricing_test.go`: Model pricing calibration
- `account_test.go`: Token refresh, cooldown, priority selection
- `kiro_api_test.go`: Profile ARN resolution, regional URL routing, BuilderId suppression

## Important Implementation Details

**Region Routing (Data-Plane):**
- Profile ARN contains the authoritative region: `arn:aws:codewhisperer:{region}:...`
- Region priority: payload profileArn → account.ProfileArn → account.Region → us-east-1
- `regionalizeURLForProfile()` rewrites hardcoded us-east-1 hosts to regional Amazon Q endpoints (`q.{region}.amazonaws.com`)
- CodeWhisperer REST endpoint only exists in us-east-1; non-us-east-1 profiles route to Q regional host
- BuilderId accounts: `ListAvailableProfiles` returns 403; suppressed for 24h after first failure; REST calls continue without profileArn

**Quota & Overages:**
- Accounts track `usageCurrent` and `usageLimit`
- When quota exhausted, account can be skipped (unless `overageStatus=ENABLED` or global `allowOverUsage=true`)
- Overages are tracked as USD charges (`currentOverages`, `overageRate`, `overageCap`)
- Per-account upstream sync happens via `auth.RefreshToken()` response metadata

**Model Aliases & Normalization:**
- Input model names (e.g., `gpt-4o`, `claude-3-5-sonnet`) are mapped to canonical Kiro model names in `translator.go`
- Version pattern: `claude-{opus|sonnet|haiku}-N-M` → `claude-{opus|sonnet|haiku}-N.M` (dash to dot)
- Aliases defined in `translator.go` at module level

**Thinking Mode:**
- Appending `-thinking` suffix to model name enables thinking mode
- Claude requests with `thinking` config also trigger it automatically
- Output format (block vs. tag) configurable in admin panel
- Thinking content is extracted from response and formatted per config

**Prompt Caching (Claude):**
- `cache_tracker.go` tracks usage of `cache_control: {"type": "ephemeral"}` annotations
- Input/output tokens are adjusted to account for cache hits/misses

**Event Stream Parsing:**
- Kiro responses use AWS EventStream (binary format with headers + payload chunks)
- Parser in `kiro.go` and `responses_handler.go` reassembles chunks into complete messages
- Messages are typed: `text`, `token`, `stop`, `error`, etc.

## Common Development Tasks

**Adding a new authentication method:**
1. Create `auth/new_method.go` with a function like `RefreshToken(token, clientID, clientSecret, ...) (accessToken, refreshToken, expiresAt, profileArn, error)`
2. Update `config.Account.AuthMethod` field if a new type is needed
3. Add routing in `auth/oidc.go` `RefreshToken()` dispatch
4. Add tests in `auth/new_method_test.go`

**Adding a new model mapping/alias:**
- Add entry to `modelAliases` slice in `proxy/translator.go`
- Optionally add version normalization pattern if needed

**Changing pricing calibration:**
- Edit `proxy/pricing.go` and `proxy/pricing_updater.go`
- Pricing table auto-updates from upstream; local cache is in `data/model_pricing.json`

**Updating the admin panel:**
- Edit files in `web/` (HTML, CSS, JS)
- No build step; files are served directly or embedded in binary
- Localization: `web/locales/en.json` and `web/locales/zh.json`

## Version & Changelog

Current version: `1.1.2` (defined in `version.json`)

## Useful Context

- **Go version:** 1.21+
- **Dependencies:** `github.com/google/uuid`, `github.com/pkoukk/tiktoken-go` + loader, `github.com/dlclark/regexp2`
- **Entry point:** `main.go` (short, handles initialization and starts HTTP server)
- **Default port:** 8080
- **Default config path:** `data/config.json` (overridable via `CONFIG_PATH` env var)
- **Admin panel:** http://localhost:8080/admin (login required)
- **API endpoints:** `/v1/messages` (Claude), `/v1/chat/completions` (OpenAI), `/v1/responses` (OpenAI Responses API), `/admin`
