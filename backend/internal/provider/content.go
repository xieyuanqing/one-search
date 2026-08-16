package provider

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

var (
	contentHTMLTagRe = regexp.MustCompile(`(?s)<[^>]+>`)
	contentSpaceRe   = regexp.MustCompile(`\s+`)
)

const noDescriptionPlaceholder = "we cannot provide a description for this page right now"

// cleanSearchContent removes presentation markup and collapses duplicated
// description/body paragraphs emitted by providers such as Brave.
func cleanSearchContent(parts ...string) string {
	paragraphs := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, paragraph := range strings.Split(part, "\n") {
			cleaned := cleanContentParagraph(paragraph)
			if cleaned == "" || strings.Contains(strings.ToLower(cleaned), noDescriptionPlaceholder) {
				continue
			}
			paragraphs = mergeContentParagraph(paragraphs, cleaned)
		}
	}
	return strings.Join(paragraphs, "\n")
}

func cleanContentParagraph(value string) string {
	value = contentHTMLTagRe.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	value = contentSpaceRe.ReplaceAllString(value, " ")
	return strings.TrimSpace(value)
}

func mergeContentParagraph(paragraphs []string, candidate string) []string {
	candidateKey := contentPrefixKey(candidate)
	if candidateKey == "" {
		return paragraphs
	}
	for index, existing := range paragraphs {
		existingKey := contentPrefixKey(existing)
		switch {
		case existingKey == candidateKey:
			if isTruncatedContent(existing) && !isTruncatedContent(candidate) {
				paragraphs[index] = candidate
			} else if isTruncatedContent(existing) == isTruncatedContent(candidate) && len([]rune(candidate)) > len([]rune(existing)) {
				paragraphs[index] = candidate
			}
			return paragraphs
		case strings.HasPrefix(existingKey, candidateKey):
			return paragraphs // Existing paragraph is the complete version.
		case strings.HasPrefix(candidateKey, existingKey):
			paragraphs[index] = candidate // Replace an earlier truncated version.
			return paragraphs
		}
	}
	return append(paragraphs, candidate)
}

func isTruncatedContent(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasSuffix(value, "...") || strings.HasSuffix(value, "…")
}

func contentPrefixKey(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimRightFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || r == '.' || r == '…'
	})
	return strings.ToLower(value)
}
