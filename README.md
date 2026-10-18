# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md)

A Go gateway for **OpenCode's anonymous free models**, based on the upstream [free-only branch](https://github.com/jasonxu114514/opencode2api/tree/free-only). It exposes Chat Completions, Responses, Anthropic Messages, and System One endpoints. This fork retains Docker/GHCR delivery and extra retry controls for rotating proxy gateways.

All upstream inference uses the OpenCode `public` credential. No Zen or Go API key is needed. Paid key pools, the WebUI, management APIs, and `sota` / `sweet` rotation groups have been removed. Configuration is read at startup; restart after editing it.

## Quick start

Build with **Go 1.24 or newer**:

```bash
git clone https://github.com/GuJi08233/opencode2api.git
cd opencode2api
cp config.example.json config.json
# Replace server_keys with your own local API key.
go build -o opencode2api ./cmd/opencode2api
./opencode2api -config config.json
```

The default API address is `127.0.0.1:8080`. `-config` selects the configuration file (default `config.json`); `-listen` overrides its API listen address. There is no management listener or port 8081.

## Docker

This fork's image is `ghcr.io/guji08233/opencode2api`, published for `linux/amd64` and `linux/arm64`. Use a release tag to pin a version, or `latest` for the latest published stable release.

```bash
cp config.example.json config.json
# Replace server_keys and configure proxies if needed.
docker compose up -d
docker compose logs -f
```

Compose exposes only port **8080** and stores configuration and caches in the `opencode2api-state` volume. The host `config.json` is imported **only on the first start**. To apply later edits, set the host file's `listen` to `0.0.0.0:8080`, copy it into the running container, then restart:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

| Compose variable | Default | Purpose |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | Image tag. |
| `OPENCODE2API_PORT` | `8080` | Host port mapped to container port 8080. |
| `OPENCODE2API_LISTEN` | Unset | Override the container API listen address. |

On first import, the entrypoint changes the example `127.0.0.1:8080` address to `0.0.0.0:8080`. Changing a host port does not change the container listener. A `proxyfile` must also be mounted inside the container at its configured path. The container runs as an unprivileged user with a read-only root filesystem and a writable state volume.

To build and start a local image:

```bash
docker build -t opencode2api:local .
docker run -d --name opencode2api \
  -p 8080:8080 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

## API

Authenticate using `Authorization: Bearer YOUR_LOCAL_API_KEY` or `x-api-key: YOUR_LOCAL_API_KEY`, matching an enabled entry in `server_keys`. These keys protect this gateway; they are not upstream credentials. `/healthz` is unauthenticated.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/models` | Currently routable free models and native protocols. |
| POST | `/v1/chat/completions` | Chat Completions. |
| POST | `/v1/responses` | Responses. |
| POST | `/v1/messages` | Anthropic Messages. |
| POST | `/v1/systemone` | Native System One structured decisions. |
| GET | `/healthz` | Readiness, model catalog, and proxy counts. |

Discover a model first, then replace `MODEL_ID` with its ID:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"

curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Hello"}]}'

curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","input":"Hello"}'

curl http://localhost:8080/v1/messages \
  -H "x-api-key: YOUR_LOCAL_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Hello"}]}'
```

Chat, Responses, and Messages support JSON and SSE; for streaming use `curl -N` and add `"stream":true`. Text, images, reasoning, and tools are translated when the target protocol supports them. Native requests preserve provider fields; cross-protocol conversion cannot preserve every option. Request bodies are limited to 32 MiB. Responses carry an `x-request-id`.

System One accepts its own `state` / `questions` payload and requires a model with `native_protocol: "systemone"`. It is forwarded without chat conversion:

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

## Free model routing

The gateway first tries the legacy `https://opencode.ai/zen/v1/*` route, then falls back to `https://opencode.ai/inference/v1/*` as needed. Both use anonymous access.

A model qualifies for anonymous routing if its ID contains `free` (case-insensitive), **or** OpenCode metadata marks both input and output prices as zero and the model is not deprecated. While metadata is unavailable or the model is missing from it, only the name rule applies. Model catalog membership, native protocol support, and availability checks further restrict what `/v1/models` exposes. An ID containing `free` passes the price test even when its metadata is deprecated; discovery still applies the other restrictions.

Model discovery refreshes every `models.refresh_seconds` (default 300). Pricing/deprecation metadata from `https://models.opencode.ai/api.json` refreshes every 24 hours. Failed refreshes retain cached data. `models.protocols` can override the native protocol with `chat`, `responses`, `anthropic`, or `systemone`.

`/healthz` returns 503 while the catalog is pending, no model is routable, or no proxy is healthy. A usable stale catalog returns 200 with degraded status. Readiness does not guarantee the next upstream inference will succeed.

## Configuration and proxies

See [config.example.json](config.example.json). JSON comments are supported. Unknown fields are ignored, including obsolete `zen_keys`, `go_keys`, `anonymous`, `prefer`, `webui`, `rotation`, and `models.free_only` settings. Free-only routing is always active. Existing `server_keys`, `proxies`, and this fork's retry options remain supported. Removed command-line flags such as `-web-listen` must be deleted from service definitions.

| Setting | Default / meaning |
| --- | --- |
| `listen` | `127.0.0.1:8080`. |
| `server_keys` | At least one enabled local key is required. |
| `upstream.zen` | `https://opencode.ai/zen`, legacy anonymous route. |
| `upstream.anonymous` | `https://opencode.ai/inference`, fallback route. |
| `proxies` / `proxyfile` | Merged and deduplicated; empty sources use `direct`. Relative file paths resolve beside the configuration. |
| `retry.max_attempts` | `3`; controls proxy visit budget when a rotating pool is present. |
| `retry.anonymous_max_attempts` | `0`; positive values cap actual anonymous HTTP requests across both upstream routes and require `disable_proxy_keep_alives: true`. |
| `retry.timeout_seconds` | `300`; total inference timeout, including streaming. |
| `performance.disable_proxy_keep_alives` | `false`; fresh connections for proxied requests, with HTTP/2 disabled. Direct connections retain pooling. |
| `performance.attempt_timeout_seconds` | `0`; use the total request timeout. A positive value limits each upstream attempt. |
| `performance.first_event_timeout_seconds` | `0`; first-event timeout disabled. |
| `performance.connect_timeout_seconds` | `5`. |
| `performance.failure_cooldown_seconds` | `15`; base cooldown for static proxy failures. |
| `performance.max_idle_conns` / `max_idle_conns_per_host` | `4096` / `1024` per transport. |
| `performance.max_conns_per_host` | `0` (unlimited). |
| `performance.idle_conn_timeout_seconds` | `120`. |
| `reasoning.effort` / `effort_by_model` | Default and per-model reasoning levels; an explicit client level takes precedence. |
| `logging.level` / `dump_request_bodies` | `info` / `false`; body dumps require debug logging. |

Proxy entries accept `direct`, `http://`, `https://`, `socks5://`, and `socks5h://`, including URL credentials. A proxy file contains one entry per line; blank lines and comments (`#`, `;`, `//`, at the start or after whitespace) are supported.

### Rotating proxy gateways

Merge these fields into your configuration, using your own credentials:

```json
{
  "proxies": ["socks5h://USER:PASSWORD@resin:2260"],
  "retry": {"max_attempts": 3, "anonymous_max_attempts": 3, "timeout_seconds": 300},
  "performance": {"disable_proxy_keep_alives": true, "attempt_timeout_seconds": 20}
}
```

A positive `anonymous_max_attempts` counts **actual HTTP requests**, including the first request, fallback between the two upstream routes, and reasoning-reference retries. All attempts share the total request timeout. It requires the global `performance.disable_proxy_keep_alives: true`; marking individual entries `#pool` does not replace that requirement. Rotating proxies may be revisited without cooling down the entire entry after one exit fails. Direct entries are visited at most once per routing pass. Once streaming output has been delivered, it is not replayed.

A fresh connection lets a rotating gateway select another exit. Exit rotation and session stickiness are controlled by that gateway.

For per-entry rotating pools, use a `#pool` suffix or `pool+` prefix:

```text
http://127.0.0.1:7890#pool
pool+socks5h://127.0.0.1:1080
```

These entries always use fresh connections, skip proxy health eviction and failure cooldown, and can be visited again. With `anonymous_max_attempts: 0`, static entries are visited at most once; when pools exist, the visit budget is at least `retry.max_attempts`. A visit may try both upstream routes, so this default visit budget is not an HTTP request count. Static proxies retain connection pooling unless the global fresh-connection option is enabled.

## Persistence and development

Keep the configuration directory writable for model catalog, pricing, and availability caches. Service logs are JSON on stdout; errors before logger initialization go to stderr. In-memory metrics reset on restart. There is no configuration editor or reload API.

The upstream free-only branch focuses on anonymous request concurrency and larger connection pools. Actual throughput depends on upstream limits, proxies, and workload.

```bash
go test ./...
go vet ./...
go build ./cmd/opencode2api
```

[GPL-3.0-or-later License](LICENSE).
