package app

import "strings"

// parseAutoFactsIntent detects very explicit Chinese "remember/forget" intents.
// We keep this conservative to avoid false positives.
//
// Supported patterns (trimmed):
//
//	记住：<fact> / 记住:<fact> / 请记住：<fact> / 帮我记住：<fact>
//	忘记：<fact> / 忘记:<fact> / 请忘记：<fact>
func parseAutoFactsIntent(input string) (action string, fact string, ok bool) {
	t := strings.TrimSpace(input)
	if t == "" {
		return "", "", false
	}

	// ---- remember ----
	for _, p := range []string{"记住：", "记住:", "请记住：", "请记住:", "帮我记住：", "帮我记住:"} {
		if strings.HasPrefix(t, p) {
			fact = strings.TrimSpace(strings.TrimPrefix(t, p))
			if fact == "" {
				return "remember", "", true
			}
			return "remember", fact, true
		}
	}

	// ---- forget ----
	for _, p := range []string{"忘记：", "忘记:", "请忘记：", "请忘记:", "帮我忘记：", "帮我忘记:"} {
		if strings.HasPrefix(t, p) {
			fact = strings.TrimSpace(strings.TrimPrefix(t, p))
			if fact == "" {
				return "forget", "", true
			}
			return "forget", fact, true
		}
	}

	return "", "", false
}
