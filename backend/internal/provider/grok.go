package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/one-search/one-search/backend/internal/model"
)

const grokDefaultModel = "grok-4.5"

const grokSearchPrompt = `你是一个联网搜索助手。请使用 web_search 工具搜索用户的问题，然后返回搜索结果。

严格要求：
- 只输出一个合法的 JSON 数组，不要任何多余文字、不要 markdown 代码块。
- 每个元素格式：{"title": 网页标题, "url": 网页链接, "snippet": 该网页的核心内容摘要(50字以内)}
- 至少返回 5 条结果，最多 %d 条。
- url 必须是真实搜索到的网页链接，不要编造。
- snippet 用中文摘要（如果原网页是英文，翻译成中文摘要）。`

type GrokProvider struct {
	*HTTPProvider
}

func NewGrokProvider(cfg Config) *GrokProvider {
	cfg.Name = model.ProviderGrok
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://127.0.0.1:8000"
	}
	return &GrokProvider{HTTPProvider: NewHTTPProvider(cfg)}
}

func (p *GrokProvider) Search(ctx context.Context, req model.SearchRequest, key model.APIKey) (model.ProviderResponse, error) {
	limit := requestLimit(req.Limit, 10, 20)
	grokModel := optionString(req.Options, "model", "grok_model")
	if grokModel == "" {
		grokModel = grokDefaultModel
	}

	prompt := fmt.Sprintf(grokSearchPrompt, limit)
	input := fmt.Sprintf("搜索问题：%s", req.Query)

	body := map[string]interface{}{
		"model": grokModel,
		"input": input,
		"tools": []map[string]interface{}{
			{"type": "web_search_preview"},
		},
		"instructions": prompt,
	}

	request, err := p.newJSONRequest(ctx, http.MethodPost, "/v1/responses", body)
	if err != nil {
		return model.ProviderResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+key.Value)

	response, err := p.client.Do(request)
	if err != nil {
		return model.ProviderResponse{}, err
	}

	payload, err := p.decodeResponse(response)
	if err != nil {
		return model.ProviderResponse{}, err
	}

	results := normalizeGrokResults(payload, req.IncludeRaw, limit)
	return model.ProviderResponse{
		Results: results,
		Usage:   usageMeasurements(model.ProviderGrok, payload),
		Raw:     payload,
	}, nil
}

func normalizeGrokResults(payload map[string]interface{}, includeRaw bool, limit int) []model.SearchResult {
	// 从 output 数组中提取 message 类型的 output_text
	outputItems := resultArray(payload, "output")
	answerText := ""
	for _, rawItem := range outputItems {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		if stringValue(item, "type") != "message" {
			continue
		}
		contents, ok := item["content"].([]interface{})
		if !ok {
			continue
		}
		for _, c := range contents {
			content := mapFromInterface(c)
			if content == nil {
				continue
			}
			if stringValue(content, "type") == "output_text" {
				answerText = stringValue(content, "text")
				break
			}
		}
		if answerText != "" {
			break
		}
	}

	if answerText == "" {
		return nil
	}

	// 清理可能的 markdown 代码块包裹
	answerText = strings.TrimSpace(answerText)
	answerText = stripCodeFences(answerText)

	// 尝试解析为 JSON 数组
	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(answerText), &items); err != nil {
		// 解析失败，尝试从 web_search_call 提取 sources 作为 fallback
		return grokFallbackSources(payload, includeRaw)
	}

	results := make([]model.SearchResult, 0, len(items))
	for index, rawItem := range items {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		url := stringValue(item, "url", "link")
		if url == "" {
			continue
		}
		result := model.SearchResult{
			Title:     stringValue(item, "title"),
			URL:       url,
			Snippet:   truncate(stringValue(item, "snippet", "description"), 1000),
			Content:   truncate(stringValue(item, "snippet", "description", "content"), 4000),
			Provider:  model.ProviderGrok,
			Providers: []string{model.ProviderGrok},
			Score:     1 / float64(index+1),
		}
		if includeRaw {
			result.Raw = item
		}
		results = append(results, result)
		if limit > 0 && len(results) >= limit {
			break
		}
	}
	return results
}

// grokFallbackSources: 当 JSON 解析失败时，从 web_search_call 的 sources 提取 URL
func grokFallbackSources(payload map[string]interface{}, includeRaw bool) []model.SearchResult {
	outputItems := resultArray(payload, "output")
	seen := map[string]bool{}
	results := []model.SearchResult{}
	for _, rawItem := range outputItems {
		item := mapFromInterface(rawItem)
		if item == nil {
			continue
		}
		if stringValue(item, "type") != "web_search_call" {
			continue
		}
		action := mapFromInterface(item["action"])
		if action == nil {
			continue
		}
		sources, ok := action["sources"].([]interface{})
		if !ok {
			continue
		}
		for index, rawSource := range sources {
			source := mapFromInterface(rawSource)
			if source == nil {
				continue
			}
			url := stringValue(source, "url")
			if url == "" || seen[url] {
				continue
			}
			seen[url] = true
			results = append(results, model.SearchResult{
				Title:     grokHostFromURL(url),
				URL:       url,
				Snippet:   "",
				Provider:  model.ProviderGrok,
				Providers: []string{model.ProviderGrok},
				Score:     1 / float64(index+1),
			})
			if includeRaw {
				results[len(results)-1].Raw = source
			}
		}
	}
	return results
}

// stripCodeFences 移除 markdown 代码块包裹
func stripCodeFences(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		// 移除开头的 ```json 或 ```
		if idx := strings.Index(text[3:], "\n"); idx >= 0 {
			text = strings.TrimSpace(text[3+idx+1:])
		}
		// 移除结尾的 ```
		if strings.HasSuffix(text, "```") {
			text = strings.TrimSpace(text[:len(text)-3])
		}
	}
	return text
}

// grokHostFromURL 从 URL 提取 host 作为 title
func grokHostFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	rawURL = strings.TrimPrefix(rawURL, "https://")
	rawURL = strings.TrimPrefix(rawURL, "http://")
	if idx := strings.Index(rawURL, "/"); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	return rawURL
}
