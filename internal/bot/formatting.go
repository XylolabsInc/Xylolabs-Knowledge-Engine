package bot

import (
	"regexp"
	"strings"
	"time"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

const (
	maxReplyLength    = 10000
	maxFileDownload   = 10 * 1024 * 1024 // 10 MB
	maxThreadHistory  = 20               // max prior messages to include as context
	maxToolIterations = 5                // max function calling round-trips

	maxTrackedThreads     = 1000
	maxNotBotThreads      = 500
	threadCleanupInterval = 10 * time.Minute
	notBotCacheTTL        = 5 * time.Minute

	// Token budget estimates (chars-based, ~3 chars per token)
	maxContextChars      = 300000 // ~100k tokens for total context
	maxKBContextChars    = 200000 // ~67k tokens for KB context
	maxFileChars         = 24000  // ~8k tokens per attached file
	maxSystemPromptChars = 250000
	maxQueryBudget       = 50000 // aggregate char budget for user query + attachments
)

var (
	reLearnBlock        = regexp.MustCompile(`(?s)===LEARN:\s*(.+?)\s*===[ \t]*\r?\n(.*?)===ENDLEARN===[ \t]*\r?\n?`)
	reLearnBlockCleanup = regexp.MustCompile(`(?s)===LEARN:.*?===ENDLEARN===[ \t]*\r?\n?`)
	reReactBlock        = regexp.MustCompile(`===REACT:\s*(\S+?)===`)

	rePlainURL = regexp.MustCompile(`https?://[^\s<>]+`)
)

// isCreationTask detects if a query likely involves document/slide/sheet creation.
func isCreationTask(query string) bool {
	creationPatterns := []string{
		"만들어", "작성해", "생성해", "작성하", "생성하", "만들",
		"create", "write", "generate", "draft",
		"문서", "보고서", "회의록", "스프레드시트", "프레젠테이션", "슬라이드",
		"시트 만", "doc 만", "노션",
	}
	lower := strings.ToLower(query)
	for _, pattern := range creationPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// isComplexQuery detects if a query requires deeper reasoning.
func isComplexQuery(query string) bool {
	complexPatterns := []string{
		// Korean analysis/comparison requests
		"분석해", "비교해", "정리해", "요약해", "브리핑", "현황",
		"장단점", "평가해", "검토해", "진단해", "리뷰해",
		// English analysis patterns
		"analyze", "compare", "summarize", "evaluate", "review",
		"pros and cons", "trade-off", "assessment",
		// Reasoning indicators
		"왜", "어떻게", "why", "how does", "how can", "how should",
		"explain why", "what are the implications",
	}
	lower := strings.ToLower(query)
	for _, pattern := range complexPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	// Long queries suggest complex intent
	if len(query) > 200 {
		return true
	}
	// Multiple questions
	if strings.Count(query, "?") >= 2 {
		return true
	}
	return false
}

// platformFormattingInstructions returns platform-specific formatting guidelines
// to be injected into the system prompt.
func platformFormattingInstructions(platform string) string {
	switch platform {
	case "discord":
		return `Use standard Markdown formatting (Discord):
- Bold: **text**. Italic: *text*. Strike: ~~text~~
- Code: ` + "`code`" + `. Code block: ` + "```code```" + `. Quote: > text
- List: "- " or "• ". Link: [display text](URL)
- Headers: ## Header (use sparingly)
- Tables are supported. Use markdown tables when presenting structured data.
- NEVER use Slack mrkdwn syntax like <URL|text> or *single asterisk bold*.`
	default: // "slack"
		return `Use Slack mrkdwn formatting (NOT standard Markdown):
- Bold: *text* (single asterisk, NOT **). Italic: _text_. Strike: ~text~
- Code: ` + "`code`" + `. Code block: ` + "```code```" + `. Quote: > text
- List: "- " or "• ". Link: <URL|display text>
- NEVER use # headers, **bold**, [link](url), or other standard Markdown syntax.
- NEVER use markdown tables (| col | col |). Slack does not render tables. Use bullet-point lists instead.`
	}
}

// mergeConsecutiveRoles combines adjacent messages with the same role,
// as the Gemini API requires strictly alternating user/model turns.
func mergeConsecutiveRoles(messages []gemini.Message) []gemini.Message {
	if len(messages) == 0 {
		return messages
	}
	merged := []gemini.Message{messages[0]}
	for _, msg := range messages[1:] {
		last := &merged[len(merged)-1]
		if msg.Role == last.Role {
			last.Content += "\n\n" + msg.Content
			last.Images = append(last.Images, msg.Images...)
		} else {
			merged = append(merged, msg)
		}
	}
	return merged
}
