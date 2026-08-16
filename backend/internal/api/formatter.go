package api

import (
	"github.com/one-search/one-search/backend/internal/model"
)

// 蓝图 §7 — 格式化层
// 格式化只在最末端做一次,不要散进各个 source adapter。
// 内部管线(RRF、verify、freshness)继续用完整的胖结构,供日志和监控使用;
// 返回给模型的瘦结构由 formatter 单独生成。两边解耦。

// formatSearchResponse:末端单一 formatter [蓝图 §1 定稿]
func formatSearchResponse(response model.SearchResponse, req model.SearchRequest) interface{} {
	format := req.ResponseFormat
	if format == "" {
		format = model.FormatCompact
	}

	switch format {
	case model.FormatRaw:
		// raw:完全向后兼容,返回完整 payload
		return response

	case model.FormatSearchResult:
		// search_result:每个结果转成 search_result block
		// 注意 [蓝图 §7.4]:claude.ai 的 MCP 宿主会把它降级成纯 JSON 文本,
		// 等价于 compact 但白烧 token。这条路在 claude.ai 上不通。
		return formatSearchResultBlocks(response)

	default: // compact
		return formatCompact(response, req)
	}
}

// formatCompact:compact 格式 [蓝图 §7.3 定稿]
// score 和 source(多源标记)保留;verify_trace/latency_ms/resolved_policy/search_meta/date_parsed 移进 debug,非 debug 时不发。
func formatCompact(response model.SearchResponse, req model.SearchRequest) map[string]interface{} {
	results := make([]map[string]interface{}, 0, len(response.Results))
	for _, r := range response.Results {
		item := map[string]interface{}{
			"title":  r.Title,
			"url":    r.URL,
			"score":  r.Score,
			"source": r.Providers,
		}
		content := r.Content
		if content == "" {
			content = r.Snippet
		}
		if content != "" {
			item["content"] = content
		}
		if r.PublishedAt != nil {
			item["date"] = r.PublishedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		if req.IncludeRaw && r.Raw != nil {
			item["raw"] = r.Raw
		}
		results = append(results, item)
	}

	payload := map[string]interface{}{
		"query":  req.Query,
		"count":  len(results),
		"results": results,
	}
	if response.Answer != "" {
		payload["answer"] = response.Answer
	}

	// resolved_policy 必须出现在顶层 [蓝图 §8 行为契约 6]
	if response.ResolvedPolicy != nil {
		payload["resolved_policy"] = response.ResolvedPolicy
	}

	// warnings [蓝图 §5.2]
	if len(response.Meta.Warnings) > 0 {
		payload["warnings"] = response.Meta.Warnings
	}

	// debug 模式:塞入 policy/latency/verify_trace [蓝图 §2.1 debug 参数]
	if response.Debug != nil {
		payload["debug"] = response.Debug
	}

	return payload
}

// formatSearchResultBlocks:search_result 格式 [蓝图 §7.4]
func formatSearchResultBlocks(response model.SearchResponse) []map[string]interface{} {
	blocks := make([]map[string]interface{}, 0, len(response.Results))
	for _, r := range response.Results {
		block := map[string]interface{}{
			"type":  "search_result",
			"title": r.Title,
			"url":   r.URL,
		}
		if r.Snippet != "" {
			block["snippet"] = r.Snippet
		}
		if r.Content != "" {
			block["content"] = r.Content
		}
		if r.PublishedAt != nil {
			block["date"] = r.PublishedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		blocks = append(blocks, block)
	}
	return blocks
}
