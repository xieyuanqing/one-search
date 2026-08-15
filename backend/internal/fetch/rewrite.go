package fetch

import (
	"regexp"
	"strings"
)

// 蓝图 §6.2 — URL 重写规则
// 目的:让抓取拿到干净正文,规避导航栏/重复文件列表/注入的攻击文本。

var githubRepoPattern = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/?$`)

var discourseTopicPattern = regexp.MustCompile(`^https?://([^/]+)/t/([^/]+)/(\d+)(?:/.*)?$`)

// rewriteURL:应用 URL 重写规则 [蓝图 §6.2]
// 返回 (rewrittenURL, applied, reason)
func rewriteURL(rawURL string) (string, bool, string) {
	trimmed := strings.TrimSpace(rawURL)
	trimmed = strings.TrimSuffix(trimmed, "/")

	// GitHub repo 首页 → raw README
	if m := githubRepoPattern.FindStringSubmatch(trimmed); m != nil {
		rewritten := "https://raw.githubusercontent.com/" + m[1] + "/" + m[2] + "/HEAD/README.md"
		return rewritten, true, "github_repo_home_to_raw_readme"
	}

	// Discourse 论坛帖 → JSON API
	if m := discourseTopicPattern.FindStringSubmatch(trimmed); m != nil {
		rewritten := "https://" + m[1] + "/t/" + m[2] + "/" + m[3] + ".json"
		return rewritten, true, "discourse_topic_to_json"
	}

	return trimmed, false, ""
}
