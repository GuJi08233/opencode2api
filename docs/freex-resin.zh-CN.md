# FreeX 请求兼容与 Resin 反向代理

本分支基于 `jasonxu114514/opencode2api`，参考
[`OpenCode-Zen-FreeX` 0.5.9](https://github.com/tkhs101/OpenCode-Zen-FreeX/tree/f4f0af5)
更新匿名免费通道。网关继续提供 Chat Completions、Responses 和 Anthropic
Messages 接口，保留 WebUI、模型发现、Key 池与代理管理。

## 免费请求形态

- 匿名推理统一使用 OpenCode 2.x `/inference`，不再回退到旧的 `/zen` 免费通道。
- 使用 OpenCode 2.1.4 的独立系统提示词和完整工具集合。客户端工具使用 `clint_`
  名称发送，响应时恢复；系统提示词说明宿主真正提供的工具，不宣传不存在的内建技能。
- 两个 OpenCode session 头使用相同的新会话 ID，默认每个客户端请求独立生成。
  同一请求的重试复用身份，网关内部路由会话仍保持稳定。
- 客户端明确提供 project ID 时保留；否则生成每请求独立的 40 位十六进制项目 ID。
- 公共通道仅发送必要的请求头。上游流式响应按客户端要求透传或合并成 JSON。
- Docker 镜像包含 Bun 1.4.3，免费推理通过其原生 `fetch` 发出。WARP 自定义拨号、
  SOCKS 代理和未安装 Bun 的二进制部署保留 Go 传输。

设置 `OPENCODE_ZEN_FREE_SESSION_AFFINITY=1` 可使免费通道继续使用稳定会话。
`OPENCODE2API_BUN_BINARY` 可指定 Bun 路径；设置为 `off` 可禁用原生 Bun 传输。
这些环境变量在进程启动时设置，修改后需要重启对应容器。

## Resin 配置

将下面的 `RESIN_TOKEN` 替换为自己的 Resin 访问令牌，保留 `/zen`、`/zen/go`
和 `/inference` 路径。真实配置保存在部署数据目录，不应提交到 Git。

```json
{
  "anonymous": true,
  "warp": { "enabled": false },
  "proxies": ["direct"],
  "proxyfile": "",
  "upstream": {
    "zen": "http://resin:2260/RESIN_TOKEN/opencode/https/opencode.ai/zen",
    "go": "http://resin:2260/RESIN_TOKEN/opencode/https/opencode.ai/zen/go",
    "anonymous": "http://resin:2260/RESIN_TOKEN/opencode/https/opencode.ai/inference"
  }
}
```

两个容器需要在同一个 Docker 网络中，使 `resin` 能解析到 Resin 容器。
反向代理填写在 `upstream`，`proxies: ["direct"]` 表示直接连接这个反向代理入口。
原生 Bun 传输不会继承进程环境中的 HTTP 代理，避免内部 Resin 地址被环境代理接管。

WebUI 的配置中心提供独立的匿名免费模型 URL 字段。旧页面未提交该字段时，后端
保留当前匿名上游地址，避免保存其他选项时恢复为官方默认地址。

## 构建和调用记录

推送 `main` 会执行 Go 测试、race 检查、静态检查，然后发布
`ghcr.io/guji08233/opencode2api:latest` 和 `sha-<完整提交哈希>`，支持
`linux/amd64` 与 `linux/arm64`。镜像带有构建提交和版本标签；部署时可固定提交标签。

每次推理尝试都会记录 INFO 级 `upstream_attempt`，含实际目标地址、HTTP 状态、
代理名称和耗时，不记录请求正文或认证头。可以用该事件核对请求是否指向 Resin。

请求兼容不代表免费额度一定可用。`429`、`403` 和上游网络错误保留为调用结果；
需要结合目标地址与实际响应判断。经 HTTP 反向代理时，OpenCode 端的 TLS 连接由
Resin 建立，本地 Bun 运行时不会改变 Resin 自身的上游 TLS 实现。

## 来源

FreeX 的提示词、工具捕获及相关请求规则来自上述提交；其 MIT 许可见
[`LICENSES/OpenCode-Zen-FreeX-MIT.txt`](../LICENSES/OpenCode-Zen-FreeX-MIT.txt)。
