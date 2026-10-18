# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md)

面向 **OpenCode 匿名免费模型**的 Go 网关，基于上游 [free-only 分支](https://github.com/jasonxu114514/opencode2api/tree/free-only)。提供 Chat Completions、Responses、Anthropic Messages 和 System One 接口。本 Fork 继续维护 Docker/GHCR 镜像，以及适用于动态代理网关的重试选项。

上游推理统一使用 OpenCode 的 `public` 凭据，无需填写 Zen 或 Go API Key。付费 Key 池、WebUI、管理 API、`sota` / `sweet` 轮转分组均已移除。配置在启动时读取，修改后需要重启。

## 快速开始

使用 **Go 1.24 或更新版本**编译：

```bash
git clone https://github.com/GuJi08233/opencode2api.git
cd opencode2api
cp config.example.json config.json
# 将 server_keys 替换为自己的本地 API Key。
go build -o opencode2api ./cmd/opencode2api
./opencode2api -config config.json
```

默认 API 地址为 `127.0.0.1:8080`。`-config` 指定配置文件，默认 `config.json`；`-listen` 覆盖配置中的 API 监听地址。没有管理监听服务或 8081 端口。

## Docker

本 Fork 的镜像为 `ghcr.io/guji08233/opencode2api`，支持 `linux/amd64`、`linux/arm64`。指定发布标签可固定版本，`latest` 指向最新发布的稳定版。

```bash
cp config.example.json config.json
# 替换 server_keys，并按需配置代理。
docker compose up -d
docker compose logs -f
```

Compose 仅映射 **8080** 端口，配置及缓存保存在 `opencode2api-state` 数据卷中。宿主机 `config.json` **只在首次启动时导入**。后续修改宿主机文件时，先将其中的 `listen` 设为 `0.0.0.0:8080`，再复制到正在运行的容器并重启：

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

| Compose 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | 镜像标签。 |
| `OPENCODE2API_PORT` | `8080` | 映射到容器 8080 的宿主机端口。 |
| `OPENCODE2API_LISTEN` | 未设置 | 覆盖容器内 API 监听地址。 |

首次导入时，入口脚本将示例地址 `127.0.0.1:8080` 改为 `0.0.0.0:8080`。修改宿主机端口不会改变容器内监听地址。使用 `proxyfile` 时，还需将代理文件挂载到容器中的配置路径。容器以非特权用户运行，根文件系统只读，状态卷可写。

本地构建并运行镜像：

```bash
docker build -t opencode2api:local .
docker run -d --name opencode2api \
  -p 8080:8080 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

## API 使用

请求使用 `Authorization: Bearer YOUR_LOCAL_API_KEY` 或 `x-api-key: YOUR_LOCAL_API_KEY`，对应 `server_keys` 中启用的本地密钥。这些密钥用于保护本服务，不作为上游凭据。`/healthz` 无需认证。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/v1/models` | 当前可路由的免费模型及原生协议。 |
| POST | `/v1/chat/completions` | Chat Completions。 |
| POST | `/v1/responses` | Responses。 |
| POST | `/v1/messages` | Anthropic Messages。 |
| POST | `/v1/systemone` | 原生 System One 结构化决策。 |
| GET | `/healthz` | 就绪状态、模型目录及代理数量。 |

先获取模型列表，再将示例中的 `MODEL_ID` 替换为返回的模型 ID：

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"

curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"你好"}]}'

curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","input":"你好"}'

curl http://localhost:8080/v1/messages \
  -H "x-api-key: YOUR_LOCAL_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"你好"}]}'
```

Chat、Responses、Messages 支持 JSON 和 SSE；流式请求使用 `curl -N`，并添加 `"stream":true`。文本、图片、推理及工具调用会在目标协议支持的范围内转换。原生协议请求保留供应商字段，跨协议转换无法保留所有选项。请求体上限为 32 MiB，响应带有 `x-request-id`。

System One 使用独立的 `state` / `questions` 请求结构，需选择 `native_protocol: "systemone"` 的模型。请求原样转发，不进行聊天协议转换：

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

## 免费模型路由

网关先尝试旧版 `https://opencode.ai/zen/v1/*` 路径，按需回退至 `https://opencode.ai/inference/v1/*`，两者均为匿名访问。

模型满足以下任一条件即可通过免费判定：ID 包含 `free`（不区分大小写），或 OpenCode 元数据同时标明输入、输出价格为零且模型未弃用。元数据未就绪或缺少该模型时，仅采用名称规则。模型目录成员关系、原生协议支持及可用性检查还会进一步限制 `/v1/models` 的结果。名称包含 `free` 的模型即使元数据标记弃用，仍能通过价格判定，但仍需满足其余路由条件。

模型发现按 `models.refresh_seconds` 定期刷新，默认 300 秒。`https://models.opencode.ai/api.json` 的价格与弃用元数据每 24 小时刷新，失败时保留已有缓存。`models.protocols` 可覆盖模型原生协议，支持 `chat`、`responses`、`anthropic`、`systemone`。

模型目录未就绪、没有可路由模型或没有健康代理时，`/healthz` 返回 503；缓存虽过期但仍可用时返回 200，并显示降级状态。就绪检查不保证下一次上游推理一定成功。

## 配置与代理

完整示例见 [config.example.json](config.example.json)。JSON 支持注释。未知字段会被忽略，包括旧的 `zen_keys`、`go_keys`、`anonymous`、`prefer`、`webui`、`rotation`、`models.free_only`。免费专用路由始终启用，原有 `server_keys`、`proxies` 和本 Fork 的重试选项仍然有效。服务启动参数中的 `-web-listen` 等已移除选项需要删除。

| 配置项 | 默认值 / 含义 |
| --- | --- |
| `listen` | `127.0.0.1:8080`。 |
| `server_keys` | 至少需要一个启用的本地密钥。 |
| `upstream.zen` | `https://opencode.ai/zen`，旧版匿名路径。 |
| `upstream.anonymous` | `https://opencode.ai/inference`，回退路径。 |
| `proxies` / `proxyfile` | 合并并去重；均为空时使用 `direct`。文件相对路径以配置目录为基准。 |
| `retry.max_attempts` | `3`；存在动态池时控制代理访问轮次预算。 |
| `retry.anonymous_max_attempts` | `0`；正数限制两条上游路径的实际匿名 HTTP 请求总次数，要求 `disable_proxy_keep_alives: true`。 |
| `retry.timeout_seconds` | `300`；推理总超时，包含流式响应。 |
| `performance.disable_proxy_keep_alives` | `false`；代理请求使用新连接，并禁用 HTTP/2。直连仍复用连接。 |
| `performance.attempt_timeout_seconds` | `0`；沿用总请求超时。正数限制单次上游尝试时长。 |
| `performance.first_event_timeout_seconds` | `0`；禁用首事件超时。 |
| `performance.connect_timeout_seconds` | `5`。 |
| `performance.failure_cooldown_seconds` | `15`；静态代理失败的基础冷却时间。 |
| `performance.max_idle_conns` / `max_idle_conns_per_host` | 每个 transport 为 `4096` / `1024`。 |
| `performance.max_conns_per_host` | `0`，不限制。 |
| `performance.idle_conn_timeout_seconds` | `120`。 |
| `reasoning.effort` / `effort_by_model` | 默认及按模型指定的推理级别；客户端显式级别优先。 |
| `logging.level` / `dump_request_bodies` | `info` / `false`；请求体日志还需启用 debug。 |

代理支持 `direct`、`http://`、`https://`、`socks5://`、`socks5h://`，可携带 URL 用户名和密码。代理文件每行一项，支持空行，以及位于行首或空白之后的 `#`、`;`、`//` 注释。

### 动态代理网关

使用自己的代理凭据，将以下字段合并到现有配置：

```json
{
  "proxies": ["socks5h://USER:PASSWORD@resin:2260"],
  "retry": {"max_attempts": 3, "anonymous_max_attempts": 3, "timeout_seconds": 300},
  "performance": {"disable_proxy_keep_alives": true, "attempt_timeout_seconds": 20}
}
```

正数 `anonymous_max_attempts` 统计**实际 HTTP 请求次数**，包括首请求、两条上游路径之间的回退和失效推理引用重试，所有尝试共享总请求超时。此模式要求全局 `performance.disable_proxy_keep_alives: true`，单个代理的 `#pool` 标记不能替代该要求。动态代理可以重复访问，单个出口失败不会让整个入口进入冷却；每次路由遍历中，直连项至多访问一轮。流式输出一旦已发送给客户端，就不会重放。

新连接让代理网关有机会选择新出口，实际出口轮换和会话粘性由代理网关控制。

也可用 `#pool` 后缀或 `pool+` 前缀单独标记动态代理池：

```text
http://127.0.0.1:7890#pool
pool+socks5h://127.0.0.1:1080
```

这些入口始终使用新连接，不执行代理健康淘汰与失败冷却，且允许再次访问。`anonymous_max_attempts: 0` 时，静态入口至多访问一轮；存在动态池时，访问轮次预算至少为 `retry.max_attempts`。每轮可能尝试两条上游路径，因此默认轮次预算不等于 HTTP 请求次数。未启用全局新连接选项时，静态代理仍保留连接复用。

## 持久化与开发

配置目录需可写，以保存模型目录、价格及可用性缓存。正常服务日志以 JSON 输出到标准输出，日志器初始化前的错误输出到标准错误。内存统计在重启后清空。此版本没有配置编辑器或重载 API。

上游 free-only 分支着重优化匿名请求并发及扩大连接池，实际吞吐取决于上游限流、代理和工作负载。

```bash
go test ./...
go vet ./...
go build ./cmd/opencode2api
```

[GPL-3.0-or-later 许可证](LICENSE)。
