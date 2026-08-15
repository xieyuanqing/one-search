package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/one-search/one-search/backend/internal/model"
)

var (
	githubRepoRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/?$`)
	discourseRe  = regexp.MustCompile(`^https?://([^/]+)/t/([^/]+)/(\d+)(?:/.*)?$`)
)

type Fetcher struct {
	client *http.Client
}

func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// RewriteURL implements Blueprint §6.2 URL rewriting rules
func (f *Fetcher) RewriteURL(rawURL string) (string, *model.RewriteAttempt) {
	attempt := &model.RewriteAttempt{
		Original: rawURL,
		Applied:  false,
	}

	// 1. GitHub repo -> raw README.md
	if m := githubRepoRe.FindStringSubmatch(rawURL); len(m) == 3 {
		owner := m[1]
		repo := m[2]
		rewritten := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/HEAD/README.md", owner, repo)
		attempt.Rewritten = rewritten
		attempt.Applied = true
		attempt.Reason = "github_repo_to_raw_readme"
		return rewritten, attempt
	}

	// 2. Discourse topic -> /t/{id}.json
	if m := discourseRe.FindStringSubmatch(rawURL); len(m) == 4 {
		host := m[1]
		topicID := m[3]
		rewritten := fmt.Sprintf("https://%s/t/%s.json", host, topicID)
		attempt.Rewritten = rewritten
		attempt.Applied = true
		attempt.Reason = "discourse_topic_to_json"
		return rewritten, attempt
	}

	return rawURL, attempt
}

// Fetch handles single URL fetching through remote-first pipeline (Blueprint §6.1, §6.3, §6.4, §6.5)
func (f *Fetcher) Fetch(ctx context.Context, req model.FetchRequest) model.FetchResult {
	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		return model.FetchResult{Error: "url is required"}
	}

	maxChars := req.MaxChars
	if maxChars <= 0 {
		maxChars = 6000 // Blueprint §6.4 recommended default
	}

	fetchURL, rewriteAttempt := f.RewriteURL(targetURL)

	logs := make([]string, 0)
	traces := make([]model.FetchTraceStep, 0)

	var rawContent string
	var extractor string
	var err error

	// If direct raw github or json url rewritten, fetch directly
	if rewriteAttempt.Applied {
		logs = append(logs, fmt.Sprintf("url_rewritten: %s -> %s (%s)", req.URL, fetchURL, rewriteAttempt.Reason))
		rawContent, extractor, err = f.fetchDirect(ctx, fetchURL, &traces)
	}

	// Fallback pipeline: markdown.new -> Jina Reader
	if err != nil || !rewriteAttempt.Applied {
		if req.RemoteFirst || true {
			// Step 1: try markdown.new
			rawContent, extractor, err = f.fetchMarkdownNew(ctx, fetchURL, &traces)
			if err != nil || rawContent == "" {
				logs = append(logs, fmt.Sprintf("markdown_new_failed: %v, falling back to jina reader", err))
				// Step 2: fallback to Jina Reader (r.jina.ai)
				rawContent, extractor, err = f.fetchJinaReader(ctx, fetchURL, &traces)
				if err != nil {
					logs = append(logs, fmt.Sprintf("jina_reader_failed: %v", err))
				}
			}
		}
	}

	if err != nil && rawContent == "" {
		return model.FetchResult{
			URL:            targetURL,
			Extractor:      "failed",
			Error:          err.Error(),
			Logs:           logs,
			FetchTrace:     traces,
			RewriteAttempt: rewriteAttempt,
		}
	}

	// Anti-scraping detection (Blueprint §6.5)
	if isChallengePage(rawContent) {
		return model.FetchResult{
			URL:            targetURL,
			Extractor:      "remote-challenge",
			Content:        "",
			CharsReturned:  0,
			CharsTotal:     len(rawContent),
			Truncated:      false,
			Logs:           append(logs, "anti_scraping_challenge_detected"),
			FetchTrace:     traces,
			RewriteAttempt: rewriteAttempt,
		}
	}

	// Blueprint §6.3 Offset pagination contract
	runes := []rune(rawContent)
	totalChars := len(runes)

	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > totalChars {
		offset = totalChars
	}

	remaining := runes[offset:]
	truncated := false
	nextOffset := 0

	var sliceEnd int
	if len(remaining) > maxChars {
		sliceEnd = maxChars
		truncated = true
		// Find natural boundary (Blueprint §6.4)
		for i := maxChars; i > maxChars/2 && i > 0; i-- {
			if remaining[i] == '\n' || remaining[i] == '\r' {
				sliceEnd = i
				break
			}
		}
		nextOffset = offset + sliceEnd
	} else {
		sliceEnd = len(remaining)
		truncated = false
	}

	finalText := strings.TrimSpace(string(remaining[:sliceEnd]))
	extractMode := req.ExtractMode
	if extractMode == "" {
		extractMode = "default"
	}

	offsetScope := fmt.Sprintf("%s|%s|%s", targetURL, extractMode, extractor)

	return model.FetchResult{
		URL:            targetURL,
		Content:        finalText,
		CharsReturned:  len([]rune(finalText)),
		CharsTotal:     totalChars,
		Truncated:      truncated,
		NextOffset:     nextOffset,
		OffsetScope:    offsetScope,
		Extractor:      extractor,
		Logs:           logs,
		FetchTrace:     traces,
		RewriteAttempt: rewriteAttempt,
	}
}

func (f *Fetcher) fetchDirect(ctx context.Context, targetURL string, traces *[]model.FetchTraceStep) (string, string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("User-Agent", "OneSearchFetcher/0.1")

	resp, err := f.client.Do(httpReq)
	if err != nil {
		*traces = append(*traces, model.FetchTraceStep{Step: "direct_fetch", Status: "error", Message: err.Error()})
		return "", "", err
	}
	defer resp.Body.Close()

	*traces = append(*traces, model.FetchTraceStep{Step: "direct_fetch", Status: "success", HTTPStatus: resp.StatusCode})
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("direct fetch http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", "", err
	}
	return string(body), "direct_rewritten", nil
}

func (f *Fetcher) fetchMarkdownNew(ctx context.Context, targetURL string, traces *[]model.FetchTraceStep) (string, string, error) {
	escaped := url.QueryEscape(targetURL)
	reqURL := "https://markdown.new/" + escaped
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("User-Agent", "OneSearchFetcher/0.1")

	resp, err := f.client.Do(httpReq)
	if err != nil {
		*traces = append(*traces, model.FetchTraceStep{Step: "markdown_new", Status: "error", Message: err.Error()})
		return "", "", err
	}
	defer resp.Body.Close()

	*traces = append(*traces, model.FetchTraceStep{Step: "markdown_new", Status: "status", HTTPStatus: resp.StatusCode})
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("markdown.new http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", "", err
	}
	return string(body), "markdown.new", nil
}

func (f *Fetcher) fetchJinaReader(ctx context.Context, targetURL string, traces *[]model.FetchTraceStep) (string, string, error) {
	reqURL := "https://r.jina.ai/" + targetURL
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("User-Agent", "OneSearchFetcher/0.1")
	httpReq.Header.Set("Accept", "text/plain, text/markdown")

	resp, err := f.client.Do(httpReq)
	if err != nil {
		*traces = append(*traces, model.FetchTraceStep{Step: "jina_reader", Status: "error", Message: err.Error()})
		return "", "", err
	}
	defer resp.Body.Close()

	*traces = append(*traces, model.FetchTraceStep{Step: "jina_reader", Status: "status", HTTPStatus: resp.StatusCode})
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("jina reader http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", "", err
	}
	return string(body), "jina-reader", nil
}

func (f *Fetcher) FetchMany(ctx context.Context, req model.FetchManyRequest) model.FetchManyResult {
	results := make([]model.FetchResult, len(req.URLs))
	var wg sync.WaitGroup

	maxChars := req.MaxCharsPerPage
	if maxChars <= 0 {
		maxChars = 6000
	}

	for i, u := range req.URLs {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			results[idx] = f.Fetch(ctx, model.FetchRequest{
				URL:         target,
				MaxChars:    maxChars,
				RemoteFirst: req.RemoteFirst,
			})
		}(i, u)
	}
	wg.Wait()

	return model.FetchManyResult{Results: results}
}

func isChallengePage(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "cloudflare") && (strings.Contains(lower, "turnstile") || strings.Contains(lower, "just a moment") || strings.Contains(lower, "challenge-running") || strings.Contains(lower, "cf-browser-verification"))
}
