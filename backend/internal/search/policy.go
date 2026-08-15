package search

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/one-search/one-search/backend/internal/model"
)

// 蓝图 §3 — 策略层核心
// 设计哲学:决策从 consumer 侧搬到服务端的固定规则里。
// 调用方只给 query,顶多再给一个 intent。规则是一张能一眼看懂的表。

// Policy dimensions resolved by the strategy layer
type policyDimensions struct {
	mode       model.SearchMode
	sources    []string
	freshness  string
	modePol    string
	sourcePol  string
	freshPol   string
}

// defaultPolicy:无 intent 时的基线 [蓝图 §3.1 实测]
var defaultPolicy = policyDimensions{
	mode:      model.SearchModeDeep,
	sources:   []string{model.ProviderBrave, model.ProviderGrok},
	freshness: "",
}

// intentDefaults:七条 intent 映射 [蓝图 §3.2 实测]
var intentDefaults = map[model.SearchIntent]policyDimensions{
	model.IntentFactual: {
		mode:      model.SearchModeFast,
		sources:   []string{model.ProviderBrave},
		freshness: "",
		modePol:   "intent:factual", sourcePol: "intent:factual", freshPol: "intent:factual",
	},
	model.IntentNews: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderTavily, model.ProviderGrok},
		freshness: "pw",
		modePol:   "intent:news", sourcePol: "intent:news", freshPol: "intent:news",
	},
	model.IntentStatus: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderTavily, model.ProviderGrok},
		freshness: "pw",
		modePol:   "intent:status", sourcePol: "intent:status", freshPol: "intent:status",
	},
	model.IntentTutorial: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderGrok},
		freshness: "",
		modePol:   "intent:tutorial", sourcePol: "intent:tutorial", freshPol: "intent:tutorial",
	},
	model.IntentComparison: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderGrok},
		freshness: "",
		modePol:   "intent:comparison", sourcePol: "intent:comparison", freshPol: "intent:comparison",
	},
	model.IntentExploratory: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderGrok},
		freshness: "",
		modePol:   "intent:exploratory", sourcePol: "intent:exploratory", freshPol: "intent:exploratory",
	},
	model.IntentResource: {
		mode:      model.SearchModeDeep,
		sources:   []string{model.ProviderBrave, model.ProviderGrok},
		freshness: "",
		modePol:   "intent:resource", sourcePol: "intent:resource", freshPol: "intent:resource",
	},
}

// intentWhy:纯 default 或纯 intent 的自然语言解释 [蓝图 §3.5]
var intentWhy = map[model.SearchIntent]string{
	model.IntentFactual:     "factual: fast mode, brave only, no freshness — single fact lookup, low latency",
	model.IntentNews:        "news: deep mode, brave+tavily+grok, freshness=pw — recent news, week-level recency",
	model.IntentStatus:      "status: deep mode, brave+tavily+grok, freshness=pw — current status check",
	model.IntentTutorial:    "tutorial: deep mode, brave+grok, no freshness — official/authoritative pages, relaxed recency",
	model.IntentComparison:  "comparison: deep mode, brave+grok, no freshness — multi-source comparison",
	model.IntentExploratory: "exploratory: deep mode, brave+grok, no freshness — broad exploration",
	model.IntentResource:    "resource: deep mode, brave+grok, no freshness — resource finding, recall-oriented",
}

const defaultWhy = "default: deep mode, brave+grok, no freshness — baseline policy"

// resolvePolicy:策略解析层 [蓝图 §3.3 覆盖规则]
// 显式 mode/sources/freshness 各自独立覆盖对应维度。
func resolvePolicy(req model.SearchRequest) (policyDimensions, *model.ResolvedPolicy) {
	dim := defaultPolicy
	modePol := "default"
	sourcePol := "default"
	freshPol := "default"

	// intent 覆盖 default
	if req.Intent != "" {
		if intent, ok := intentDefaults[req.Intent]; ok {
			dim = intent
			modePol = dim.modePol
			sourcePol = dim.sourcePol
			freshPol = dim.freshPol
			if modePol == "" {
				modePol = "intent:" + string(req.Intent)
			}
			if sourcePol == "" {
				sourcePol = "intent:" + string(req.Intent)
			}
			if freshPol == "" {
				freshPol = "intent:" + string(req.Intent)
			}
		}
	}

	// 显式参数按维度独立覆盖 [蓝图 §3.3]
	if req.ModeExplicit && req.Mode != "" {
		dim.mode = req.Mode
		modePol = "explicit"
	}
	if req.ProvidersExplicit && len(req.Providers) > 0 {
		dim.sources = req.Providers
		sourcePol = "explicit"
	}
	if req.FreshnessExplicit && req.Freshness != "" {
		dim.freshness = req.Freshness
		freshPol = "explicit"
	}

	// mode=answer 强制 sources=["tavily"] [蓝图 §3.3]
	if dim.mode == model.SearchModeAnswer {
		dim.sources = []string{model.ProviderTavily}
		sourcePol = "mode:answer"
	}

	// why 字段两形态 [蓝图 §3.5]
	why := buildWhy(req, modePol, sourcePol, freshPol)

	policy := &model.ResolvedPolicy{
		Policy:          classifyPolicy(modePol, sourcePol, freshPol),
		Mode:            dim.mode,
		Sources:         append([]string(nil), dim.sources...),
		Freshness:       dim.freshness,
		DomainBoost:     req.DomainBoost,
		ModePolicy:      modePol,
		SourcePolicy:    sourcePol,
		FreshnessPolicy: freshPol,
		Why:             why,
	}

	return dim, policy
}

// buildWhy:why 字段的两种形态 [蓝图 §3.5 实测,曾修过 bug]
func buildWhy(req model.SearchRequest, modePol, sourcePol, freshPol string) string {
	// 纯 default 或纯 intent,无任何显式覆盖 → 自然语言解释
	if modePol != "explicit" && sourcePol != "explicit" && freshPol != "explicit" {
		if req.Intent != "" {
			if why, ok := intentWhy[req.Intent]; ok {
				return why
			}
		}
		return defaultWhy
	}
	// 任意维度有显式覆盖 → 切换成中性的维度来源摘要
	return fmt.Sprintf("resolved from mixed policies: mode_policy=%s; source_policy=%s; freshness_policy=%s",
		modePol, sourcePol, freshPol)
}

func classifyPolicy(modePol, sourcePol, freshPol string) string {
	hasExplicit := modePol == "explicit" || sourcePol == "explicit" || freshPol == "explicit"
	hasModeAnswer := modePol == "mode:answer" || sourcePol == "mode:answer"
	if hasExplicit || hasModeAnswer {
		return "mixed"
	}
	if modePol == "default" && sourcePol == "default" && freshPol == "default" {
		return "default"
	}
	return "intent"
}

// detectCJK:中文检测 → CJK 路由 [蓝图 §9-P1-4]
// 中文时效性查询上 Tavily 的中文源覆盖显著强于 Brave
func detectCJK(query string) bool {
	cjkCount := 0
	for _, r := range query {
		if unicode.Is(unicode.Han, r) {
			cjkCount++
		}
	}
	return cjkCount > 0
}

// applyCJKRouting:CJK 查询走 Tavily 优先 [蓝图 §9-P1-4]
func applyCJKRouting(dim *policyDimensions) {
	if !detectCJK(dim.sourcesJoined()) {
		return
	}
	// 检查是否已经在 sources 里包含 tavily
	for _, s := range dim.sources {
		if s == model.ProviderTavily {
			return
		}
	}
	// CJK 查询且非 fast 模式:把 tavily 加到 sources 前面
	if dim.mode != model.SearchModeFast {
		dim.sources = append([]string{model.ProviderTavily}, dim.sources...)
	}
}

func (d policyDimensions) sourcesJoined() string {
	return strings.Join(d.sources, ",")
}
