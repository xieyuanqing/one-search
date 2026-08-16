# One Search MCP 使用指南

One Search 是一个面向 AI 客户端的远程搜索与网页提取 MCP 服务。它提供搜索、批量搜索、网页正文提取和运行状态查询能力。

服务地址：

```text
https://mcp.nijikit.com/mcp
```

OAuth 发现地址：

```text
https://mcp.nijikit.com/.well-known/oauth-authorization-server
```

## 1. 接入方式

### 方式 A：OAuth 2.1（推荐远程客户端）

在 One Search 管理台的“OAuth 连接器”创建客户端，然后在 AI 客户端填写：

```text
MCP Server URL:
https://mcp.nijikit.com/mcp

Authorization Server:
https://mcp.nijikit.com

Scope:
search

Grant Type:
Authorization Code + PKCE (S256)
```

Claude 的回调 URI：

```text
https://claude.ai/api/mcp/auth_callback
```

创建 OAuth Client 时，必须把上面的回调 URI 加入允许列表。Client Secret 只在创建时显示一次。

OAuth 端点：

```text
Authorization: https://mcp.nijikit.com/oauth/authorize
Token:         https://mcp.nijikit.com/oauth/token
```

Access Token 有效期为 1 小时。OAuth 客户端应使用：

```http
Authorization: Bearer <access_token>
```

### 方式 B：Bearer API Token

在管理台“接口令牌”创建 API Token，然后对 MCP 请求添加：

```http
Authorization: Bearer <api_token>
Content-Type: application/json
```

API Token 可以限制 Provider、RPM、日额度和月额度。Token 只在创建时显示完整值。

## 2. MCP 握手

One Search 使用 Streamable HTTP MCP。推荐先发送 `initialize`：

```http
POST https://mcp.nijikit.com/mcp
Content-Type: application/json
Authorization: Bearer <token>
```

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-06-18",
    "capabilities": {},
    "clientInfo": {
      "name": "your-ai-client",
      "version": "1.0.0"
    }
  }
}
```

然后发送通知：

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/initialized"
}
```

支持的 MCP 协议版本：

```text
2025-06-18
2025-03-26
2024-11-05
```

也可以直接调用：

```http
GET https://mcp.nijikit.com/mcp
```

该请求会返回服务状态和工具名称列表。

## 3. 工具总览

| 工具 | 用途 | 推荐场景 |
|---|---|---|
| `search` | 单个查询搜索 | 普通事实查询、研究起点 |
| `batch_search` | 并行执行多个查询并合并 | 同一主题的多个角度、对比研究 |
| `fetch` | 提取一个网页正文 | 阅读某个重点来源 |
| `fetch_many` | 并行提取多个网页 | 批量阅读搜索结果 |
| `search_and_fetch` | 搜索后自动提取前 N 条 | 快速获得搜索结果和正文 |
| `status` | 查看 Provider 和策略状态 | 调试、确认服务能力 |

## 4. `search`

最常用的工具。只需要 `query`：

```json
{
  "query": "DeepSeek Harness official repository"
}
```

推荐的完整调用：

```json
{
  "query": "Kimi K3 open weights technical report",
  "intent": "resource",
  "mode": "deep",
  "sources": "brave,grok",
  "num": 8,
  "freshness": "pm"
}
```

### 参数

| 参数 | 类型 | 说明 |
|---|---|---|
| `query` | string | 必填。建议不超过 8 个关键词或短语。 |
| `intent` | string | `factual`、`status`、`comparison`、`tutorial`、`exploratory`、`news`、`resource`。 |
| `mode` | string | `fast`、`deep`、`answer`。 |
| `sources` | string | 逗号分隔的 Provider，例如 `brave,tavily`。显式指定后覆盖意图默认来源。 |
| `num` | integer | 返回结果数量，默认 10，最大 50。 |
| `freshness` | string | `pd` 24 小时、`pw` 7 天、`pm` 30 天、`py` 365 天。 |
| `domain_boost` | string | 提升指定域名的排序权重。 |
| `snippet_chars` | integer | snippet 最大字符数，默认 1000。 |
| `content_chars` | integer | content 最大字符数，默认 4000。 |
| `response_format` | string | `compact`、`raw`、`search_result`，默认 `compact`。 |
| `debug` | boolean | 返回额外延迟和验证信息。默认关闭。 |
| `include_raw` | boolean | 返回上游原始结果字段。默认关闭。 |

### 模式选择

- `fast`：低延迟，适合单事实或快速补充信息。
- `deep`：多源并行搜索并进行 RRF 融合，适合研究、比较和重要结论。
- `answer`：在搜索结果基础上请求带引用的答案，适合需要直接回答的问题。

如果没有明确指定 `intent` 或 `mode`，服务端会根据默认策略自动解析。需要稳定、可复现的行为时，建议显式指定 `mode` 和 `sources`。

### 典型响应

默认 `compact` 响应结构：

```json
{
  "query": "DeepSeek Harness official repository",
  "count": 2,
  "results": [
    {
      "title": "DeepSeek Harness",
      "url": "https://github.com/deepseek-ai/deepseek-harness",
      "score": 0.016393443,
      "source": ["brave"],
      "content": "..."
    }
  ],
  "resolved_policy": {
    "policy": "...",
    "mode": "deep",
    "sources": ["brave", "grok"],
    "why": "..."
  }
}
```

`source` 是数组，表示结果来自哪些 Provider。不要依赖单值 `provider` 字段。

`debug=true` 时会额外返回诊断信息，例如 Provider 延迟和 `verify_trace`。普通调用不建议打开。

## 5. `batch_search`

一次最多提交 4 个查询：

```json
{
  "queries": [
    "Kimi K3 open weights",
    "Kimi K3 technical report",
    "Kimi K3 benchmark results"
  ],
  "mode": "deep",
  "sources": "brave,grok",
  "num": 10,
  "return_buckets": false
}
```

### 重要语义

- `queries` 最多 4 条。
- `num` 是最终 `merged_results` 的总数量上限。
- 每个 query 的原始结果通常会按各自搜索请求返回。
- 合并阶段会按 query 桶轮转取结果，避免第一个 query 把全部槽位占满。
- 相同 URL 会去重并累加分数。
- `count` 是合并结果数量。
- `query_count` 是 query 数量。
- `merged_count` 与 `count` 相同，便于客户端明确理解。

返回示例：

```json
{
  "query_count": 2,
  "count": 4,
  "merged_count": 4,
  "merged_results": [
    {
      "title": "...",
      "url": "...",
      "score": 0.032,
      "source": ["brave"],
      "content": "..."
    }
  ]
}
```

只有在需要检查每个 query 的原始结果时才设置：

```json
{
  "return_buckets": true
}
```

`return_buckets=true` 会显著增加响应体积，因为会同时返回每个 query 的桶和合并结果。一般 AI 客户端应保持 `false`。

## 6. `fetch`

提取单个网页的正文：

```json
{
  "url": "https://github.com/deepseek-ai/deepseek-harness/"
}
```

默认链路：

```text
markdown.new → Jina Reader → 本机直连正文提取
```

默认情况下，`remote_first` 是 `true`，即使调用方省略这个字段也会走远程提取链。

需要显式使用本机直连时：

```json
{
  "url": "https://example.com/article",
  "remote_first": false,
  "max_chars": 8000
}
```

### 参数

| 参数 | 类型 | 说明 |
|---|---|---|
| `url` | string | 必填。完整网页 URL。 |
| `max_chars` | integer | 返回字符上限，默认 6000。 |
| `offset` | integer | 从正文的字符偏移位置继续读取。 |
| `extract_mode` | string | 提取模式保留字段，通常省略。 |
| `remote_first` | boolean | 默认 `true`；显式 `false` 才走直连。 |

响应包含：

```json
{
  "url": "https://example.com/article",
  "content": "正文 Markdown 或纯文本",
  "chars_returned": 6000,
  "chars_total": 18342,
  "truncated": true,
  "next_offset": 5987,
  "extractor": "markdown.new",
  "fetch_trace": [
    {
      "step": "markdown_new",
      "status": "status",
      "http_status": 200
    }
  ]
}
```

当 `truncated=true` 时，使用 `next_offset` 继续调用：

```json
{
  "url": "https://example.com/article",
  "offset": 5987,
  "max_chars": 6000
}
```

不要根据 `http_status=200` 就判断正文有效，应检查 `content`、`extractor` 和 `fetch_trace`。

## 7. `fetch_many`

并行提取多个 URL：

```json
{
  "urls": [
    "https://example.com/one",
    "https://example.com/two",
    "https://example.com/three"
  ],
  "max_chars_per_page": 6000
}
```

默认同样使用 `remote_first=true`。每个 URL 的成功或失败独立返回，不会因为一个页面失败而丢弃其他页面。

## 8. `search_and_fetch`

搜索并自动提取前几条结果：

```json
{
  "query": "DeepSeek Harness architecture",
  "mode": "deep",
  "sources": "brave,grok",
  "num": 8,
  "fetch_top": 3,
  "max_chars_per_page": 6000
}
```

建议：

- `fetch_top` 通常设为 2 到 3。
- 先用 `search` 筛选来源，再用 `fetch_many` 批量读取，通常更容易控制上下文长度。
- `search_and_fetch` 的默认 `remote_first` 为 `true`。

## 9. `status`

无参数调用：

```json
{}
```

可用于查看：

- 当前启用 Provider
- Provider 是否有可用 Key
- Intent 默认策略
- RRF 权重
- freshness 窗口
- 当前运行设置
- 支持的输出格式

AI 客户端在执行复杂研究任务前，可以先调用一次 `status` 了解可用能力，但不必每次普通搜索都调用。

## 10. 推荐调用策略

### 普通事实问题

```text
search(query, intent="factual", mode="fast", num=5)
```

### 最新状态或新闻

```text
search(query, intent="news", mode="deep", freshness="pw", num=8)
```

### 技术比较

```text
search(query, intent="comparison", mode="deep", sources="brave,grok", num=10)
```

### 深度研究

1. 用 `status` 确认 Provider 能力。
2. 用 `batch_search` 拆成 2 到 4 个互补 query。
3. 从 `merged_results` 选择来源。
4. 用 `fetch_many` 读取重点 URL。
5. 对关键结论保留来源 URL，不要只引用搜索摘要。

### 网页正文读取

1. 优先调用 `fetch` 或 `fetch_many`。
2. 默认保留 `remote_first=true`。
3. 检查 `truncated`。
4. 有 `next_offset` 时继续读取。
5. 遇到挑战页、空正文或失败时，换一个来源，不要把空内容当作页面没有信息。

## 11. 错误处理

常见 HTTP 状态：

| 状态 | 含义 |
|---|---|
| `200` | MCP 请求成功，具体工具错误仍需检查 JSON-RPC 内容。 |
| `400` | JSON-RPC body 或工具参数无效。 |
| `401` | 缺少或无效的 Bearer Token。 |
| `429` | API Token RPM 或额度限制。 |
| `500` | 服务端内部错误或上游异常。 |

常见 JSON-RPC 错误：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32602,
    "message": "..."
  }
}
```

AI 客户端不要把以下情况当作有效搜索结果：

- `content` 为空
- `fetch` 返回 `extractor=failed`
- `fetch_trace` 全部失败
- `truncated=true` 但没有继续读取
- 返回的是挑战页或明显的登录页

## 12. 不要使用的旧接口

当前服务使用原生 MCP Streamable HTTP：

```text
https://mcp.nijikit.com/mcp
```

不要使用旧的兼容接口、OpenAI 兼容搜索接口或未记录的 `/v1/compat/*` 路径。搜索能力统一通过上述 MCP 工具调用。
