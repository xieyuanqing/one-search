package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	if params.Name != "search" {
		return nil, mcpInvalidParams(req.ID, "unknown tool: "+params.Name)
	}

	// 蓝图复刻:解析蓝图参数面 — sources(逗号分隔字符串), num, intent, mode, domain_boost, debug 等
	var rawArgs map[string]interface{}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &rawArgs); err != nil {
			return nil, mcpInvalidParams(req.ID, "invalid search arguments")
		}
	}

	var searchReq model.SearchRequest
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &searchReq); err != nil {
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
		searchReq.ProvidersExplicit = hasJSONField(params.Arguments, "providers") || hasJSONField(params.Arguments, "sources")
	}

	// num → Limit [蓝图 §2.1: num 是 RRF 融合后的最终总结果数]
	if num, ok := rawArgs["num"].(float64); ok && num > 0 {
		searchReq.Limit = int(num)
		searchReq.LimitExplicit = true
	} else if !hasJSONField(params.Arguments, "limit") && !hasJSONField(params.Arguments, "num") {
		searchReq.LimitExplicit = false
	} else {
		searchReq.LimitExplicit = hasJSONField(params.Arguments, "limit") || hasJSONField(params.Arguments, "num")
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
		return r.Context(), http.StatusUnauthorized, "invalid api token", err
	}
	if !h.auth.allowToken(apiToken) {
		return r.Context(), http.StatusTooManyRequests, "api token rate limit exceeded", fmt.Errorf("api token rate limit exceeded")
	}
	ctx := context.WithValue(r.Context(), apiTokenIDKey, apiToken.ID)
	ctx = context.WithValue(ctx, apiTokenKey, apiToken)
	return ctx, http.StatusOK, "", nil
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
	// 蓝图复刻:六个工具。当前阶段已实现 search,其余后续阶段补齐。
	return []interface{}{
		mcpSearchToolSchema(),
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
					"enum":        []string{string(model.SearchModeFast), string(model.SearchModeDeep), string(model.SearchModeAnswer), string(model.SearchModeParallel), string(model.SearchModeFallback), string(model.SearchModeSingle)},
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
