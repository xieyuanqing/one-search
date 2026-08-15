package model

// 蓝图 §6 — 抓取层

type FetchRequest struct {
	URL          string `json:"url"`
	MaxChars     int    `json:"max_chars,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	ExtractMode  string `json:"extract_mode,omitempty"`
	RemoteFirst  bool   `json:"remote_first,omitempty"`
	Extractor    string `json:"extractor,omitempty"`
}

type FetchResult struct {
	URL            string            `json:"url"`
	Content        string            `json:"content"`
	CharsReturned  int               `json:"chars_returned"`
	CharsTotal     int               `json:"chars_total,omitempty"`
	Truncated      bool              `json:"truncated"`
	NextOffset     int               `json:"next_offset,omitempty"`
	OffsetScope    string            `json:"offset_scope,omitempty"`
	Extractor      string            `json:"extractor"`
	Title          string            `json:"title,omitempty"`
	Logs           []string          `json:"logs,omitempty"`
	FetchTrace     []FetchTraceStep  `json:"fetch_trace,omitempty"`
	RewriteAttempt *RewriteAttempt   `json:"rewrite_attempt,omitempty"`
	Frontmatter    map[string]string `json:"frontmatter,omitempty"`
	Error          string            `json:"error,omitempty"`
}

type FetchTraceStep struct {
	Step       string `json:"step"`
	Status     string `json:"status"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
}

type RewriteAttempt struct {
	Original string `json:"original"`
	Rewritten string `json:"rewritten,omitempty"`
	Applied  bool   `json:"applied"`
	Reason   string `json:"reason,omitempty"`
}

type FetchManyRequest struct {
	URLs            []string `json:"urls"`
	MaxCharsPerPage int      `json:"max_chars_per_page,omitempty"`
	RemoteFirst     bool     `json:"remote_first,omitempty"`
}

type FetchManyResult struct {
	Results []FetchResult `json:"results"`
}

// SearchAndFetch 组合:先 search 后 fetch top N
type SearchAndFetchRequest struct {
	Query           string   `json:"query"`
	Intent          string   `json:"intent,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	Sources         string   `json:"sources,omitempty"`
	Num             int      `json:"num,omitempty"`
	Freshness       string   `json:"freshness,omitempty"`
	FetchTop        int      `json:"fetch_top,omitempty"`
	MaxCharsPerPage int      `json:"max_chars_per_page,omitempty"`
	RemoteFirst     bool     `json:"remote_first,omitempty"`
	DomainBoost     string   `json:"domain_boost,omitempty"`
	Debug           bool     `json:"debug,omitempty"`
}
