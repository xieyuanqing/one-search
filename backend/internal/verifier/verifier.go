package verifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/one-search/one-search/backend/internal/fetch"
	"github.com/one-search/one-search/backend/internal/model"
)

// 蓝图 §5 — Grok verifier 修正版
// 修复已知严重缺陷(§5.3):
// 1. dead 只保留给确定性 HTTP 失败 (404 / 410 / DNS 解析失败)
// 2. verifier 抓取走 remote-first 管线 (通过 fetcher)，不误杀 SPA / Cloudflare 盾页面
// 3. 其余抓取失败一律降级为 unverified_kept 保留，不丢弃

const (
	ConsistencyThreshold = 0.7
	DeadThreshold        = 0.2
)

type Verifier struct {
	fetcher *fetch.Fetcher
}

func NewVerifier(fetcher *fetch.Fetcher) *Verifier {
	if fetcher == nil {
		fetcher = fetch.NewFetcher()
	}
	return &Verifier{fetcher: fetcher}
}

// VerifyResults executes verifier check over search results
func (v *Verifier) VerifyResults(ctx context.Context, results []model.SearchResult) ([]model.SearchResult, []model.VerifyTrace, []string) {
	verified := make([]model.SearchResult, 0, len(results))
	traces := make([]model.VerifyTrace, 0, len(results))
	warnings := make([]string, 0)

	for _, item := range results {
		// Quick check via remote-first fetch
		fetchRes := v.fetcher.Fetch(ctx, model.FetchRequest{
			URL:         item.URL,
			MaxChars:    2000,
			RemoteFirst: true,
		})

		// 1. 确定性 HTTP 失败判定为 dead (404/410/DNS)
		if isDeterministicDead(fetchRes) {
			traces = append(traces, model.VerifyTrace{
				URL:         item.URL,
				Provider:    item.Provider,
				Consistency: 0.0,
				Status:      "dead_dropped",
				Reason:      fetchRes.Error,
			})
			warnings = append(warnings, fmt.Sprintf("dead_dropped:%s", item.URL))
			continue // 丢弃 dead 链接
		}

		// 2. 内容一致性打分
		consistency := calculateConsistency(item, fetchRes.Content)

		if consistency >= ConsistencyThreshold {
			traces = append(traces, model.VerifyTrace{
				URL:         item.URL,
				Provider:    item.Provider,
				Consistency: consistency,
				Status:      "verified",
			})
			verified = append(verified, item)
		} else if consistency < DeadThreshold && fetchRes.Error != "" {
			// 仅在明确抓取失败且分极低时丢弃
			traces = append(traces, model.VerifyTrace{
				URL:         item.URL,
				Provider:    item.Provider,
				Consistency: consistency,
				Status:      "dead_dropped",
				Reason:      "low_consistency_and_fetch_failed",
			})
			warnings = append(warnings, fmt.Sprintf("dead_dropped:%s", item.URL))
		} else {
			// 其余情况一律降级保留 (unverified_kept)
			traces = append(traces, model.VerifyTrace{
				URL:         item.URL,
				Provider:    item.Provider,
				Consistency: consistency,
				Status:      "unverified_kept",
			})
			verified = append(verified, item)
		}
	}

	return verified, traces, warnings
}

func isDeterministicDead(res model.FetchResult) bool {
	if res.Error == "" {
		return false
	}
	errLower := strings.ToLower(res.Error)
	return strings.Contains(errLower, "404") || strings.Contains(errLower, "410") || strings.Contains(errLower, "no such host")
}

func calculateConsistency(item model.SearchResult, body string) float64 {
	if body == "" {
		return 0.5 // Default neutral if remote challenge or empty
	}
	titleWords := strings.Fields(strings.ToLower(item.Title))
	if len(titleWords) == 0 {
		return 0.8
	}

	bodyLower := strings.ToLower(body)
	hit := 0
	for _, w := range titleWords {
		if len(w) > 2 && strings.Contains(bodyLower, w) {
			hit++
		}
	}
	ratio := float64(hit) / float64(len(titleWords))
	if ratio > 1.0 {
		return 1.0
	}
	return ratio
}
