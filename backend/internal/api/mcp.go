package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/one-search/one-search/backend/internal/model"
)

const (
	mcpLatestProtocolVersion  = "2025-06-18"
	mcpDefaultProtocolVersion = "2025-03-26"
)

var mcpSupportedProtocolVersions = []string{mcpLatestProtocolVersion, mcpDefaultProtocolVersion, "2024-11-05"}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (h *Handler) mountMCP(r chi.Router, path string) {
	h.mountMCPPath(r, path)
	if strings.HasSuffix(path, "/") {
		trimmed := strings.TrimRight(path, "/")
		if trimmed != "" {
			h.mountMCPPath(r, trimmed)
		}
		return
	}
	h.mountMCPPath(r, path+"/")
}

func (h *Handler) mountMCPPath(r chi.Router, path string) {
	r.Get(path, h.mcpInfo)
	r.Post(path, h.mcp)
	r.Delete(path, h.mcpDelete)
}

func (h *Handler) mcpInfo(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":                     true,
		"transport":                   "streamable-http",
		"protocol_version":            mcpLatestProtocolVersion,
		"supported_protocol_versions": mcpSupportedProtocolVersions,
		"endpoint":                    r.URL.Path,
		"auth":                        "Authorization: Bearer *** or X-API-Key",
		"tools":                       []string{"search", "batch_search", "fetch", "fetch_many", "search_and_fetch", "status"},
	})
}

func (h *Handler) mcpDelete(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "invalid body", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "empty json-rpc body", nil)
		return
	}

	if trimmed[0] == '[' {
		var requests []mcpRequest
		if err := json.Unmarshal(trimmed, &requests); err != nil {
			writeMCPError(w, http.StatusBadRequest, nil, -32700, "parse error", err.Error())
			return
		}
		if h.mcpRequestsRequireAuth(requests) {
			ctx, authStatus, authMessage, err := h.mcpAuthContext(r)
			if err != nil {
				h.writeMCPAuthChallenge(w, r, authStatus)
				writeMCPError(w, authStatus, firstMCPRequestID(trimmed), -32001, authMessage, nil)
				return
			}
			r = r.WithContext(ctx)
		}
		h.handleMCPBatch(w, r, requests)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "parse error", err.Error())
		return
	}
	if h.mcpRequestsRequireAuth([]mcpRequest{req}) {
		ctx, authStatus, authMessage, err := h.mcpAuthContext(r)
		if err != nil {
			h.writeMCPAuthChallenge(w, r, authStatus)
			writeMCPError(w, authStatus, req.ID, -32001, authMessage, nil)
			return
		}
		r = r.WithContext(ctx)
	}
	response, ok := h.handleMCPRequest(r, req)
	if !ok {
		writeMCPAccepted(w)
		return
	}
	writeMCPResponse(w, http.StatusOK, response)
}

func (h *Handler) handleMCPBatch(w http.ResponseWriter, r *http.Request, requests []mcpRequest) {
	if len(requests) == 0 {
		writeMCPError(w, http.StatusBadRequest, nil, -32600, "empty batch is not allowed", nil)
		return
	}
	responses := make([]mcpResponse, 0, len(requests))
	for _, req := range requests {
		response, ok := h.handleMCPRequest(r, req)
		if ok {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		writeMCPAccepted(w)
		return
	}
	writeMCPBatchResponse(w, http.StatusOK, responses)
}

func (h *Handler) handleMCPRequest(r *http.Request, req mcpRequest) (mcpResponse, bool) {
	if req.ID == nil {
		h.handleMCPNotification(r, req)
		return mcpResponse{}, false
	}
	if req.JSONRPC != "2.0" {
		return newMCPError(req.ID, -32600, "jsonrpc must be 2.0", nil), true
	}
	if req.Method == "" {
		return newMCPError(req.ID, -32600, "method is required", nil), true
	}

	switch req.Method {
	case "initialize":
		return newMCPResult(req.ID, mcpInitializeResult(req.Params)), true
	case "ping":
		return newMCPResult(req.ID, map[string]interface{}{}), true
	case "tools/list":
		return newMCPResult(req.ID, map[string]interface{}{"tools": mcpAllToolSchemas()}), true
	case "tools/call":
		result, errResp := h.handleMCPToolCall(r, req)
		if errResp != nil {
			return *errResp, true
		}
		return newMCPResult(req.ID, result), true
	case "resources/list", "prompts/list":
		key := "resources"
		if req.Method == "prompts/list" {
			key = "prompts"
		}
		return newMCPResult(req.ID, map[string]interface{}{key: []interface{}{}}), true
	case "resources/templates/list":
		return newMCPResult(req.ID, map[string]interface{}{"resourceTemplates": []interface{}{}}), true
	default:
		return newMCPError(req.ID, -32601, "method not found", req.Method), true
	}
}

func (h *Handler) handleMCPNotification(r *http.Request, req mcpRequest) {
	_ = r
	_ = req
}

func (h *Handler) handleMCPToolCall(r *http.Request, req mcpRequest) (interface{}, *mcpResponse) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) == 0 {
		return nil, mcpInvalidParams(req.ID, "params are required")
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, mcpInvalidParams(req.ID, "invalid params")
	}

	switch params.Name {
	case "search":
		return h.handleMCPSearch(r, req, params.Arguments)
	case "fetch":
		return h.handleMCPFetch(r, req, params.Arguments)
	case "fetch_many":
		return h.handleMCPFetchMany(r, req, params.Arguments)
	case "search_and_fetch":
		return h.handleMCPSearchAndFetch(r, req, params.Arguments)
	case "batch_search":
		return h.handleMCPBatchSearch(r, req, params.Arguments)
	case "status":
		return h.handleMCPStatus(r, req)
	default:
		return nil, mcpInvalidParams(req.ID, "unknown tool: "+params.Name)
	}
}

func (h *Handler) handleMCPSearch(r *http.Request, req mcpRequest, args json.RawMessage) (interface{}, *mcpResponse) {
	var rawArgs map[string]interface{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &rawArgs); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid search arguments")
		}
	}

	var searchReq model.SearchRequest
	if len(args) > 0 {
		if err := json.Unmarshal(args, &searchReq); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid search arguments")
		}
	}
	searchReq.Query = strings.TrimSpace(searchReq.Query)
	if searchReq.Query == "" {
		return nil, mcpInvalidParams(req.ID, "query is required")
	}

	// 蓝图参数映射:sources(逗号分隔) → Providers
	if sourcesStr, ok := rawArgs["sources"].(string); ok && sourcesStr != "" {
		parts := strings.Split(sourcesStr, ",")
		providers := []string{}
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				providers = append(providers, p)
			}
		}
		if len(providers) > 0 {
			searchReq.Providers = providers
			searchReq.ProvidersExplicit = true
		}
	} else if len(searchReq.Providers) > 0 {
		searchReq.ProvidersExplicit = hasJSONField(args, "providers") || hasJSONField(args, "sources")
	}

	// num → Limit [蓝图 §2.1: num 是 RRF 融合后的最终总结果数]
	if num, ok := rawArgs["num"].(float64); ok && num > 0 {
		searchReq.Limit = int(num)
		searchReq.LimitExplicit = true
	} else if !hasJSONField(args, "limit") && !hasJSONField(args, "num") {
		searchReq.LimitExplicit = false
	} else {
		searchReq.LimitExplicit = hasJSONField(args, "limit") || hasJSONField(args, "num")
	}

	// mode 显式标记
	if modeStr, ok := rawArgs["mode"].(string); ok && modeStr != "" {
		searchReq.Mode = model.SearchMode(modeStr)
		searchReq.ModeExplicit = true
	}

	// freshness 显式标记
	if freshStr, ok := rawArgs["freshness"].(string); ok && freshStr != "" {
		searchReq.Freshness = freshStr
		searchReq.FreshnessExplicit = true
	}

	// intent
	if intentStr, ok := rawArgs["intent"].(string); ok && intentStr != "" {
		searchReq.Intent = model.SearchIntent(intentStr)
	}

	// domain_boost
	if boostStr, ok := rawArgs["domain_boost"].(string); ok {
		searchReq.DomainBoost = boostStr
	}

	// snippet_chars / content_chars
	if sc, ok := rawArgs["snippet_chars"].(float64); ok {
		searchReq.SnippetChars = int(sc)
	}
	if cc, ok := rawArgs["content_chars"].(float64); ok {
		searchReq.ContentChars = int(cc)
	}

	// response_format
	if rf, ok := rawArgs["response_format"].(string); ok && rf != "" {
		searchReq.ResponseFormat = model.ResponseFormat(rf)
	}

	// debug
	if dbg, ok := rawArgs["debug"].(bool); ok {
		searchReq.Debug = dbg
	}

	searchReq.CompatFormat = model.CompatFormatNative
	if searchReq.Options == nil {
		searchReq.Options = map[string]interface{}{}
	}
	searchReq.Options["source"] = "mcp"

	if token, ok := APIToken(r.Context()); ok {
		filtered, err := applyTokenProviders(searchReq.Providers, token.AllowedProviders)
		if err != nil {
			return nil, &mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: -32003, Message: err.Error()}}
		}
		searchReq.Providers = filtered
	}

	response, err := h.orchestrator.Search(r.Context(), searchReq, RequestID(r.Context()), APITokenID(r.Context()))
	if err != nil {
		return mcpToolError(err.Error()), nil
	}

	// 蓝图复刻:格式化层 [蓝图 §7] — compact(默认)/ raw / search_result
	formatted := formatSearchResponse(response, searchReq)
	payload, err := json.MarshalIndent(formatted, "", "  ")
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": formatted,
		"isError":           false,
	}, nil
}

func (h *Handler) handleMCPFetch(r *http.Request, req mcpRequest, args json.RawMessage) (interface{}, *mcpResponse) {
	var fetchReq model.FetchRequest
	if len(args) > 0 {
		if err := json.Unmarshal(args, &fetchReq); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid fetch arguments")
		}
	}
	if strings.TrimSpace(fetchReq.URL) == "" {
		return nil, mcpInvalidParams(req.ID, "url is required")
	}
	if !hasJSONField(args, "remote_first") {
		fetchReq.RemoteFirst = true
	}

	result := h.fetcher.Fetch(r.Context(), fetchReq)
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": result,
		"isError":           result.Error != "",
	}, nil
}

func (h *Handler) handleMCPFetchMany(r *http.Request, req mcpRequest, args json.RawMessage) (interface{}, *mcpResponse) {
	var fetchManyReq model.FetchManyRequest
	if len(args) > 0 {
		if err := json.Unmarshal(args, &fetchManyReq); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid fetch_many arguments")
		}
	}
	if len(fetchManyReq.URLs) == 0 {
		return nil, mcpInvalidParams(req.ID, "urls is required")
	}
	if !hasJSONField(args, "remote_first") {
		fetchManyReq.RemoteFirst = true
	}

	result := h.fetcher.FetchMany(r.Context(), fetchManyReq)
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": result,
		"isError":           false,
	}, nil
}

func (h *Handler) handleMCPSearchAndFetch(r *http.Request, req mcpRequest, args json.RawMessage) (interface{}, *mcpResponse) {
	var sfReq model.SearchAndFetchRequest
	if len(args) > 0 {
		if err := json.Unmarshal(args, &sfReq); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid search_and_fetch arguments")
		}
	}
	if strings.TrimSpace(sfReq.Query) == "" {
		return nil, mcpInvalidParams(req.ID, "query is required")
	}
	if !hasJSONField(args, "remote_first") {
		sfReq.RemoteFirst = true
	}

	searchArgs, _ := json.Marshal(model.SearchRequest{
		Query:       sfReq.Query,
		Intent:      model.SearchIntent(sfReq.Intent),
		Mode:        model.SearchMode(sfReq.Mode),
		Freshness:   sfReq.Freshness,
		Limit:       sfReq.Num,
		DomainBoost: sfReq.DomainBoost,
		Debug:       sfReq.Debug,
	})

	searchRes, errResp := h.handleMCPSearch(r, req, searchArgs)
	if errResp != nil {
		return nil, errResp
	}

	searchMap, ok := searchRes.(map[string]interface{})
	if !ok {
		return searchRes, nil
	}

	fetchTop := sfReq.FetchTop
	if fetchTop <= 0 {
		fetchTop = 2 // Blueprint §2.3 recommended 2-3
	}

	// Extract URLs from search results
	urls := make([]string, 0)
	if structured, ok := searchMap["structuredContent"].(map[string]interface{}); ok {
		if results, ok := structured["results"].([]map[string]interface{}); ok {
			for i, res := range results {
				if i >= fetchTop {
					break
				}
				if u, ok := res["url"].(string); ok && u != "" {
					urls = append(urls, u)
				}
			}
		}
	}

	fetchResults := h.fetcher.FetchMany(r.Context(), model.FetchManyRequest{
		URLs:            urls,
		MaxCharsPerPage: sfReq.MaxCharsPerPage,
		RemoteFirst:     sfReq.RemoteFirst,
	})

	combined := map[string]interface{}{
		"search":  searchMap["structuredContent"],
		"fetched": fetchResults.Results,
	}

	payload, _ := json.MarshalIndent(combined, "", "  ")
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": combined,
		"isError":           false,
	}, nil
}

type batchQueryBucket struct {
	Query    string      `json:"query"`
	Response interface{} `json:"response"`
}

func (h *Handler) handleMCPBatchSearch(r *http.Request, req mcpRequest, args json.RawMessage) (interface{}, *mcpResponse) {
	var rawArgs struct {
		Queries       []string `json:"queries"`
		ReturnBuckets bool     `json:"return_buckets"`
		Num           int      `json:"num"`
		Intent        string   `json:"intent"`
		Mode          string   `json:"mode"`
		Sources       string   `json:"sources"`
		Freshness     string   `json:"freshness"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &rawArgs); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid batch_search arguments")
		}
	}

	if len(rawArgs.Queries) == 0 {
		return nil, mcpInvalidParams(req.ID, "queries array is required")
	}
	if len(rawArgs.Queries) > 4 {
		rawArgs.Queries = rawArgs.Queries[:4] // Blueprint §2.2: max 4 queries
	}

	buckets := make([]batchQueryBucket, len(rawArgs.Queries))
	var wg sync.WaitGroup

	for i, q := range rawArgs.Queries {
		wg.Add(1)
		go func(idx int, queryStr string) {
			defer wg.Done()
			qArgs, _ := json.Marshal(map[string]interface{}{
				"query":     queryStr,
				"intent":    rawArgs.Intent,
				"mode":      rawArgs.Mode,
				"sources":   rawArgs.Sources,
				"freshness": rawArgs.Freshness,
				"num":       rawArgs.Num,
			})
			res, _ := h.handleMCPSearch(r, req, qArgs)
			if resMap, ok := res.(map[string]interface{}); ok {
				buckets[idx] = batchQueryBucket{Query: queryStr, Response: resMap["structuredContent"]}
			}
		}(i, q)
	}
	wg.Wait()

	merged := mergeBatchBuckets(buckets, rawArgs.Num)
	result := map[string]interface{}{
		"query_count":  len(buckets),
		"count":        len(merged),
		"merged_count": len(merged),
		"merged_results": merged,
	}
	if rawArgs.ReturnBuckets {
		result["buckets"] = buckets
	}

	payload, _ := json.MarshalIndent(result, "", "  ")
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": result,
		"isError":           false,
	}, nil
}

func mergeBatchBuckets(buckets []batchQueryBucket, limit int) []map[string]interface{} {
	if limit <= 0 {
		limit = 10
	}

	bucketItems := make([][]map[string]interface{}, len(buckets))
	for bucketIndex, bucket := range buckets {
		payload, ok := bucket.Response.(map[string]interface{})
		if !ok {
			continue
		}
		for _, rawItem := range resultItems(payload["results"]) {
			item, ok := rawItem.(map[string]interface{})
			if !ok || canonicalResultURL(item) == "" {
				continue
			}
			clone := make(map[string]interface{}, len(item)+1)
			for key, value := range item {
				clone[key] = value
			}
			clone["_bucket_index"] = bucketIndex
			bucketItems[bucketIndex] = append(bucketItems[bucketIndex], clone)
		}
		sort.SliceStable(bucketItems[bucketIndex], func(i, j int) bool {
			return resultScore(bucketItems[bucketIndex][i]) > resultScore(bucketItems[bucketIndex][j])
		})
	}

	positions := make([]int, len(bucketItems))
	merged := make(map[string]map[string]interface{})
	result := make([]map[string]interface{}, 0, limit)
	for len(result) < limit {
		progress := false
		for bucketIndex, items := range bucketItems {
			for positions[bucketIndex] < len(items) {
				item := items[positions[bucketIndex]]
				positions[bucketIndex]++
				canonical := canonicalResultURL(item)
				if existing, found := merged[canonical]; found {
					existing["score"] = resultScore(existing) + resultScore(item)
					continue
				}
				delete(item, "_bucket_index")
				merged[canonical] = item
				result = append(result, item)
				progress = true
				break
			}
			if len(result) >= limit {
				break
			}
		}
		if !progress {
			break
		}
	}
	return result
}

func resultItems(value interface{}) []interface{} {
	switch items := value.(type) {
	case []interface{}:
		return items
	case []map[string]interface{}:
		out := make([]interface{}, len(items))
		for i := range items {
			out[i] = items[i]
		}
		return out
	default:
		return nil
	}
}

func canonicalResultURL(item map[string]interface{}) string {
	rawURL, _ := item["url"].(string)
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(rawURL)), "/")
}

func resultScore(item map[string]interface{}) float64 {
	score, _ := item["score"].(float64)
	return score
}

func (h *Handler) handleMCPStatus(r *http.Request, req mcpRequest) (interface{}, *mcpResponse) {
	providers, _ := h.store.ListProviders(r.Context())
	settings, _ := h.store.RuntimeSettings(r.Context())

	provNames := make([]string, 0)
	for _, p := range providers {
		if p.Enabled {
			provNames = append(provNames, p.Name)
		}
	}

	intentDefaults := []map[string]interface{}{
		{"intent": "factual", "mode": "fast", "sources": []string{"brave"}, "freshness": nil, "why": "single fact lookup, low latency"},
		{"intent": "status", "mode": "deep", "sources": []string{"brave", "tavily", "grok"}, "freshness": "pw", "why": "current status check"},
		{"intent": "comparison", "mode": "deep", "sources": []string{"brave", "grok"}, "freshness": nil, "why": "multi-source comparison"},
		{"intent": "tutorial", "mode": "deep", "sources": []string{"brave", "grok"}, "freshness": nil, "why": "official and authoritative pages"},
		{"intent": "exploratory", "mode": "deep", "sources": []string{"brave", "grok"}, "freshness": nil, "why": "broad exploration"},
		{"intent": "news", "mode": "deep", "sources": []string{"brave", "tavily", "grok"}, "freshness": "pw", "why": "recent news"},
		{"intent": "resource", "mode": "deep", "sources": []string{"brave", "grok"}, "freshness": nil, "why": "resource finding and recall"},
	}

	statusMap := map[string]interface{}{
		"server": "one-search-relay",
		"version": "1.0.0-blueprint-v1",
		"providers_available": provNames,
		"provider_flags": map[string]bool{
			"brave": providerEnabled(providers, model.ProviderBrave),
			"tavily": providerEnabled(providers, model.ProviderTavily),
			"tavily_url": providerEnabled(providers, model.ProviderTavily),
			"exa": providerEnabled(providers, model.ProviderExa),
			"grok": providerEnabled(providers, model.ProviderGrok),
		},
		"default_policy": map[string]interface{}{
			"mode": "deep",
			"sources": []string{"brave", "grok"},
			"freshness": nil,
			"why": "default: deep mode, brave+grok, no freshness — baseline policy",
		},
		"intent_defaults": intentDefaults,
		"rrf_weights": map[string]float64{
			"grok": 1.2,
			"brave": 1.0,
			"exa": 1.0,
			"tavily": 0.6,
		},
		"domain_boost_multiplier": 1.5,
		"grok_verifier": map[string]float64{"consistency_threshold": 0.7, "dead_threshold": 0.2},
		"thick_fetch_threshold": nil,
		"stale_score_multiplier": nil,
		"output_formats": []string{"compact", "raw", "search_result"},
		"freshness_windows": map[string]string{"pd": "24h", "pw": "7d", "pm": "30d", "py": "365d"},
		"sources_param": map[string]string{"type": "comma-separated string", "default": "resolved by intent or default policy"},
		"runtime_settings": settings,
	}

	payload, _ := json.MarshalIndent(statusMap, "", "  ")
	return map[string]interface{}{
		"content":           []mcpContent{{Type: "text", Text: string(payload)}},
		"structuredContent": statusMap,
		"isError":           false,
	}, nil
}

func (h *Handler) mcpRequestsRequireAuth(requests []mcpRequest) bool {
	for _, req := range requests {
		if mcpMethodRequiresAuth(req.Method) {
			return true
		}
	}
	return false
}

func mcpMethodRequiresAuth(method string) bool {
	switch method {
	case "", "initialize", "notifications/initialized", "ping", "tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return false
	default:
		return true
	}
}

func (h *Handler) mcpAuthContext(r *http.Request) (context.Context, int, string, error) {
	settings, err := h.store.RuntimeSettings(r.Context())
	if err != nil {
		return r.Context(), http.StatusInternalServerError, err.Error(), err
	}
	if !settings.APIAuthRequired {
		return r.Context(), http.StatusOK, "", nil
	}
	token := bearerToken(r)
	if token == "" {
		return r.Context(), http.StatusUnauthorized, "api token required", fmt.Errorf("api token required")
	}
	adminKey, ok, err := h.store.FindAdminAPIKey(r.Context(), token)
	if err != nil {
		return r.Context(), http.StatusInternalServerError, err.Error(), err
	}
	if ok {
		return context.WithValue(r.Context(), adminActorKey, adminAPIKeyActor(adminKey)), http.StatusOK, "", nil
	}
	apiToken, err := h.store.FindAPIToken(r.Context(), token)
	if err != nil {
		apiToken, err = h.store.FindOAuthAccessToken(r.Context(), token)
		if err != nil {
			return r.Context(), http.StatusUnauthorized, "invalid api token", err
		}
	}
	if !h.auth.allowToken(apiToken) {
		return r.Context(), http.StatusTooManyRequests, "api token rate limit exceeded", fmt.Errorf("api token rate limit exceeded")
	}
	ctx := context.WithValue(r.Context(), apiTokenIDKey, apiToken.ID)
	ctx = context.WithValue(ctx, apiTokenKey, apiToken)
	return ctx, http.StatusOK, "", nil
}

func (h *Handler) writeMCPAuthChallenge(w http.ResponseWriter, r *http.Request, status int) {
	if status != http.StatusUnauthorized {
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+oauthIssuer(r)+`/.well-known/oauth-protected-resource/mcp"`)
}

func mcpInitializeResult(params json.RawMessage) map[string]interface{} {
	return map[string]interface{}{
		"protocolVersion": negotiateMCPProtocolVersion(params),
		"capabilities": map[string]interface{}{
			"tools":     map[string]interface{}{"listChanged": false},
			"resources": map[string]interface{}{"listChanged": false},
			"prompts":   map[string]interface{}{"listChanged": false},
		},
		"serverInfo": map[string]interface{}{
			"name":    "one-search-relay",
			"title":   "One Search Relay",
			"version": "0.1.0",
		},
		"instructions": "Use tools/call with the search tool to run web search through configured One Search Relay providers.",
	}
}

func negotiateMCPProtocolVersion(params json.RawMessage) string {
	if len(params) == 0 {
		return mcpDefaultProtocolVersion
	}
	var payload struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return mcpDefaultProtocolVersion
	}
	for _, version := range mcpSupportedProtocolVersions {
		if payload.ProtocolVersion == version {
			return version
		}
	}
	return mcpDefaultProtocolVersion
}

func mcpAllToolSchemas() []interface{} {
	// 蓝图复刻:六个工具
	return []interface{}{
		mcpSearchToolSchema(),
		mcpFetchToolSchema(),
		mcpFetchManyToolSchema(),
		mcpSearchAndFetchToolSchema(),
		mcpBatchSearchToolSchema(),
		mcpStatusToolSchema(),
	}
}

func mcpFetchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "fetch",
		"title":       "Fetch Webpage",
		"description": "Fetch and extract clean markdown content from a URL via remote-first pipeline (markdown.new -> Jina Reader).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"url": map[string]interface{}{
					"type":        "string",
					"description": "The URL to fetch.",
				},
				"max_chars": map[string]interface{}{
					"type":        "integer",
					"description": "Max characters to return. Default 6000.",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Character offset for pagination/continuation.",
				},
				"extract_mode": map[string]interface{}{
					"type":        "string",
					"description": "Extraction mode.",
				},
				"remote_first": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to prioritize remote extractor.",
				},
			},
			"required": []string{"url"},
		},
	}
}

func mcpFetchManyToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "fetch_many",
		"title":       "Fetch Multiple Webpages",
		"description": "Concurrently fetch multiple URLs with extraction and offset tracking.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"urls": map[string]interface{}{
					"type":        "array",
					"description": "List of URLs to fetch.",
					"items":       map[string]interface{}{"type": "string"},
				},
				"max_chars_per_page": map[string]interface{}{
					"type":        "integer",
					"description": "Max characters per page. Default 6000.",
				},
				"remote_first": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to prioritize remote extractor.",
				},
			},
			"required": []string{"urls"},
		},
	}
}

func mcpSearchAndFetchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "search_and_fetch",
		"title":       "Search and Fetch Top Results",
		"description": "Run search and immediately fetch full text for top N results.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query.",
				},
				"intent": map[string]interface{}{
					"type":        "string",
					"description": "Search intent.",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Search mode (fast/deep/answer).",
				},
				"sources": map[string]interface{}{
					"type":        "string",
					"description": "Comma-separated providers.",
				},
				"num": map[string]interface{}{
					"type":        "integer",
					"description": "Total search results count.",
				},
				"freshness": map[string]interface{}{
					"type":        "string",
					"description": "Freshness filter.",
				},
				"fetch_top": map[string]interface{}{
					"type":        "integer",
					"description": "Number of top results to fetch. Default 2.",
				},
				"max_chars_per_page": map[string]interface{}{
					"type":        "integer",
					"description": "Max chars per fetched page. Default 6000.",
				},
				"domain_boost": map[string]interface{}{
					"type":        "string",
					"description": "Domain boost pattern.",
				},
				"debug": map[string]interface{}{
					"type":        "boolean",
					"description": "Include debug info.",
				},
			},
			"required": []string{"query"},
		},
	}
}

func mcpBatchSearchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "batch_search",
		"title":       "Batch Search Queries",
		"description": "Concurrently execute up to 4 non-overlapping queries and merge with RRF.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"queries": map[string]interface{}{
					"type":        "array",
					"description": "List of mutually exclusive search queries (max 4).",
					"items":       map[string]interface{}{"type": "string"},
				},
				"return_buckets": map[string]interface{}{
					"type":        "boolean",
					"description": "Whether to return raw per-query result buckets.",
				},
				"num": map[string]interface{}{
					"type":        "integer",
					"description": "Per-bucket and total merged results cap. Default 10.",
				},
				"intent": map[string]interface{}{
					"type":        "string",
					"description": "Search intent.",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Search mode.",
				},
				"sources": map[string]interface{}{
					"type":        "string",
					"description": "Comma-separated providers.",
				},
				"freshness": map[string]interface{}{
					"type":        "string",
					"description": "Freshness filter.",
				},
			},
			"required": []string{"queries"},
		},
	}
}

func providerEnabled(providers []model.ProviderConfig, name string) bool {
	for _, provider := range providers {
		if provider.Name == name {
			return provider.Enabled && provider.AvailableKeys > 0
		}
	}
	return false
}

func mcpStatusToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        "status",
		"title":       "Server Status and Config",
		"description": "Return full server capabilities, intent defaults, active providers, and RRF weights.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func mcpSearchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":  "search",
		"title": "One Search",
		"description": "Search the web through configured providers with intent-driven policy, weighted RRF fusion, and optional verification.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query. Recommended ≤8 words.",
				},
				"intent": map[string]interface{}{
					"type":        "string",
					"description": "Search intent. Drives mode/sources/freshness automatically.",
					"enum":        []string{string(model.IntentFactual), string(model.IntentStatus), string(model.IntentComparison), string(model.IntentTutorial), string(model.IntentExploratory), string(model.IntentNews), string(model.IntentResource)},
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Search mode. fast=low latency single source; deep=multi-source parallel+RRF; answer=AI answer with citations.",
					"enum":        []string{string(model.SearchModeFast), string(model.SearchModeDeep), string(model.SearchModeAnswer)},
				},
				"sources": map[string]interface{}{
					"type":        "string",
					"description": "Comma-separated providers, e.g. \"brave,exa\". Overrides intent-derived sources.",
				},
				"num": map[string]interface{}{
					"type":        "integer",
					"description": "Total results after RRF fusion. Default 10, max 50.",
					"minimum":     1,
					"maximum":     50,
				},
				"freshness": map[string]interface{}{
					"type":        "string",
					"description": "Freshness filter: pd(24h), pw(week), pm(month), py(year).",
					"enum":        []string{"pd", "pw", "pm", "py"},
				},
				"domain_boost": map[string]interface{}{
					"type":        "string",
					"description": "Domain to boost (1.5× score multiplier).",
				},
				"snippet_chars": map[string]interface{}{
					"type":        "integer",
					"description": "Max snippet chars. Default 1000.",
				},
				"content_chars": map[string]interface{}{
					"type":        "integer",
					"description": "Max content chars. Default 4000.",
				},
				"response_format": map[string]interface{}{
					"type":        "string",
					"description": "Output format: compact (default), raw (full payload), search_result.",
					"enum":        []string{string(model.FormatCompact), string(model.FormatRaw), string(model.FormatSearchResult)},
				},
				"debug": map[string]interface{}{
					"type":        "boolean",
					"description": "Return resolved_policy, per-source latency, verify_trace.",
				},
				"include_raw": map[string]interface{}{
					"type":        "boolean",
					"description": "Include raw upstream result items.",
				},
			},
			"required": []string{"query"},
		},
		"annotations": map[string]interface{}{
			"title":         "One Search",
			"readOnlyHint":  true,
			"openWorldHint": true,
		},
	}
}

func mcpToolError(message string) map[string]interface{} {
	return map[string]interface{}{
		"content": []mcpContent{{Type: "text", Text: message}},
		"isError": true,
	}
}

func mcpInvalidParams(id json.RawMessage, message string) *mcpResponse {
	response := newMCPError(id, -32602, message, nil)
	return &response
}

func newMCPResult(id json.RawMessage, result interface{}) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func newMCPError(id json.RawMessage, code int, message string, data interface{}) mcpResponse {
	if id == nil {
		id = json.RawMessage("null")
	}
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message, Data: data}}
}

func writeMCPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data interface{}) {
	writeMCPResponse(w, status, newMCPError(id, code, message, data))
}

func writeMCPAccepted(w http.ResponseWriter) {
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(http.StatusAccepted)
}

func writeMCPResponse(w http.ResponseWriter, status int, response mcpResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func writeMCPBatchResponse(w http.ResponseWriter, status int, responses []mcpResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Mcp-Protocol-Version", mcpLatestProtocolVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(responses)
}

func firstMCPRequestID(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if body[0] == '[' {
		var requests []mcpRequest
		if err := json.Unmarshal(body, &requests); err == nil && len(requests) > 0 {
			return requests[0].ID
		}
		return nil
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err == nil {
		return req.ID
	}
	return nil
}
