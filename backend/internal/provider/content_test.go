package provider

import "testing"

func TestCleanSearchContentPrefersCompleteParagraph(t *testing.T) {
	content := cleanSearchContent(
		"Kimi K3 reaches <strong>2.8 trillion</strong> parameters and continues pushing the scale frontier...",
		"Kimi K3 reaches 2.8 trillion parameters and continues pushing the scale frontier.",
		"A separate useful paragraph.",
	)
	want := "Kimi K3 reaches 2.8 trillion parameters and continues pushing the scale frontier.\nA separate useful paragraph."
	if content != want {
		t.Fatalf("cleanSearchContent() = %q, want %q", content, want)
	}
}

func TestCleanSearchContentRemovesPlaceholderAndEntities(t *testing.T) {
	content := cleanSearchContent(
		"We cannot provide a description for this page right now.",
		"GLM-5.3&#x27;s release notes &amp; benchmarks",
	)
	want := "GLM-5.3's release notes & benchmarks"
	if content != want {
		t.Fatalf("cleanSearchContent() = %q, want %q", content, want)
	}
}
