package search

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/one-search/one-search/backend/internal/model"
)

// 蓝图 §4 — 加权 RRF 融合层
// score(doc) = Σ_source  w_source × 1 / (k + rank_source)
// k = 60。必须先做 URL 归一化去重,再把同 URL 的各源分数求和,最后按合并分排序。

const rrfK = 60

// providerWeights:各源权重 [蓝图 §4.2 实测]
var providerWeights = map[string]float64{
	model.ProviderGrok:    1.2, // X/Twitter 实时内容、VTuber 动态
	model.ProviderBrave:   1.0, // 基准源:官方文档、权威媒体、GitHub,最快
	model.ProviderExa:     1.0, // 语义检索,opt-in
	model.ProviderTavily:  0.6, // 长尾:Reddit/Medium/论坛/YouTube,质量最低但偶有独占
	model.ProviderYou:     1.0,
	model.ProviderJina:    1.0,
	model.ProviderSerper:  1.0,
	model.ProviderFirecrawl: 1.0,
}

const domainBoostMultiplier = 1.5 // [蓝图 §4.3 实测]

// rrfMergedResult:RRF 融合后的结果
type rrfMergedResult struct {
	model.SearchResult
	rrfScore   float64
	providers  []string
}

// mergeWithRRF:加权 RRF 融合 [蓝图 §4.1]
// 输入:各 provider 的执行结果(已按各源 rank 排序)
// 输出:RRF 融合 + domain_boost + 截断后的结果
func mergeWithRRF(executions []providerExecution, req model.SearchRequest) ([]model.SearchResult, int, []string) {
	// 1. 收集每个源的有序结果列表,计算 RRF 分数
	type sourceResult struct {
		url       string
		rank      int // 0-indexed
		provider  string
		result    model.SearchResult
	}

	urlToMerged := map[string]*rrfMergedResult{}
	var order []string // 保持首次出现顺序
	warnings := []string{}

	for _, exec := range executions {
		if exec.err != nil {
			// [蓝图 §9-P1-6] warnings 暴露源失败
			warnings = append(warnings, fmt.Sprintf("source_failed:%s:%s", exec.provider, exec.errorType))
			continue
		}
		weight := providerWeights[exec.provider]
		if weight == 0 {
			weight = 1.0
		}
		for rank, result := range exec.results {
			canonical := canonicalURL(result.URL)
			if canonical == "" {
				continue
			}

			// RRF 分数: w_source × 1 / (k + rank_source)
			rrfScore := weight * (1.0 / float64(rrfK+rank+1))

			if existing, ok := urlToMerged[canonical]; ok {
				// 同 URL 的各源分数求和 [蓝图 §4.1 必须先做 URL 归一化去重]
				existing.rrfScore += rrfScore
				existing.providers = appendIfMissing(existing.providers, exec.provider)
				// 保留更长的 snippet/content
				if len(result.Snippet) > len(existing.Snippet) {
					existing.Snippet = result.Snippet
				}
				if len(result.Content) > len(existing.Content) {
					existing.Content = result.Content
				}
				if result.Title != "" && existing.Title == "" {
					existing.Title = result.Title
				}
				if result.PublishedAt != nil && existing.PublishedAt == nil {
					existing.PublishedAt = result.PublishedAt
				}
			} else {
				merged := &rrfMergedResult{
					SearchResult: result,
					rrfScore:     rrfScore,
					providers:    []string{exec.provider},
				}
				if len(merged.Providers) == 0 && merged.Provider != "" {
					merged.Providers = []string{merged.Provider}
				}
				urlToMerged[canonical] = merged
				order = append(order, canonical)
			}
		}
	}

	// 2. domain_boost 乘数 [蓝图 §4.3]
	boostDomain := strings.TrimSpace(req.DomainBoost)
	if boostDomain != "" {
		boostDomain = strings.ToLower(boostDomain)
		for _, canonical := range order {
			merged := urlToMerged[canonical]
			parsed, err := url.Parse(merged.URL)
			if err == nil && strings.Contains(strings.ToLower(parsed.Host), boostDomain) {
				merged.rrfScore *= domainBoostMultiplier
			}
		}
	}

	// 3. 转为切片并按 RRF 分数降序排序
	merged := make([]*rrfMergedResult, 0, len(order))
	for _, canonical := range order {
		merged = append(merged, urlToMerged[canonical])
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].rrfScore == merged[j].rrfScore {
			return merged[i].Title < merged[j].Title
		}
		return merged[i].rrfScore > merged[j].rrfScore
	})

	// 4. 每源最低配额 [蓝图 §9-P1-5]
	// merged_results 按 num 总量封顶会把低权重源全挤掉
	// → 加 per-source 最小保留名额
	merged = applyMinQuota(merged, executions, req.Limit)

	// 5. 截断到 limit
	deduped := len(order) - len(merged)
	if req.Limit > 0 && len(merged) > req.Limit {
		merged = merged[:req.Limit]
	}

	// 6. 写回 Score 和 Providers
	results := make([]model.SearchResult, 0, len(merged))
	for _, m := range merged {
		m.Score = m.rrfScore
		m.Providers = m.providers
		if req.SnippetChars > 0 && len(m.Snippet) > req.SnippetChars {
			m.Snippet = truncateAtBoundary(m.Snippet, req.SnippetChars)
		}
		if req.ContentChars > 0 && len(m.Content) > req.ContentChars {
			m.Content = truncateAtBoundary(m.Content, req.ContentChars)
		}
		results = append(results, m.SearchResult)
	}

	return results, deduped, warnings
}

// applyMinQuota:每源最低保留名额 [蓝图 §9-P1-5]
// 确保低权重源(如 Tavily 0.6)至少有 1 条结果进入最终列表
func applyMinQuota(merged []*rrfMergedResult, executions []providerExecution, limit int) []*rrfMergedResult {
	if limit <= 0 {
		return merged
	}

	// 统计每个成功源的结果数
	successSources := map[string]bool{}
	for _, exec := range executions {
		if exec.err == nil && len(exec.results) > 0 {
			successSources[exec.provider] = true
		}
	}
	if len(successSources) <= 1 {
		return merged
	}

	// 每源至少保留 1 条(如果 limit 允许)
	minPerSource := 1
	totalMin := len(successSources) * minPerSource
	if totalMin >= limit {
		// limit 太小,无法保证配额,直接返回
		return merged
	}

	// 已选中的结果中,统计各源是否已有结果
	inList := map[string]bool{}
	for _, m := range merged {
		for _, p := range m.providers {
			inList[p] = true
		}
	}

	// 找出没有结果入选的源
	missingSources := []string{}
	for s := range successSources {
		if !inList[s] {
			missingSources = append(missingSources, s)
		}
	}

	if len(missingSources) == 0 {
		return merged
	}

	// 从 merged 末尾往前找,把缺失源的结果换入
	// 保留区:最后 len(missingSources) 个位置给缺失源
	reserveCount := len(missingSources)
	if reserveCount >= len(merged) {
		return merged
	}
	keepUpto := len(merged) - reserveCount
	result := make([]*rrfMergedResult, 0, len(merged))

	// 先收集已入选的(到 keepUpto 为止)
	for i := 0; i < keepUpto; i++ {
		result = append(result, merged[i])
	}

	// 从 merged[keepUpto:] 中按源挑
	inserted := map[string]bool{}
	tail := merged[keepUpto:]
	for _, missing := range missingSources {
		found := false
		for _, m := range tail {
			if inserted[missing] {
				break
			}
			for _, p := range m.providers {
				if p == missing && !inserted[missing] {
					result = append(result, m)
					inserted[missing] = true
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}
	// 补齐剩余位置
	for _, m := range tail {
		if len(result) >= len(merged) {
			break
		}
		// 跳过已插入的
		alreadyIn := false
		for _, r := range result {
			if r == m {
				alreadyIn = true
				break
			}
		}
		if !alreadyIn {
			result = append(result, m)
		}
	}

	return result
}

func appendIfMissing(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

// truncateAtBoundary:截断到最近的段落/句子边界 [蓝图 §6.4]
// 用 rune 遍历以正确处理 CJK 等多字节字符
func truncateAtBoundary(text string, max int) string {
	if max <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	// 先尝试段落边界(向前找 \n)
	cutoff := max
	for i := max; i > max/2 && i > 0; i-- {
		if runes[i] == '\n' || runes[i] == '\r' {
			cutoff = i
			break
		}
	}
	// 再退到句子边界
	for i := cutoff; i > max/2 && i > 0; i-- {
		c := runes[i]
		if c == '.' || c == '!' || c == '?' || c == '。' || c == '！' || c == '？' {
			cutoff = i + 1
			break
		}
	}
	return strings.TrimSpace(string(runes[:cutoff]))
}
