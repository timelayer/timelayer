package app

import (
	"database/sql"
	"strings"
	"time"
)

// maybeAutoProposePendingFromUserInput tries to capture simple, high-signal
// self-statements (e.g. "我最喜欢的颜色是黄色") and proposes them into
// FACTS → PENDING silently.
//
// Design goals:
//   - Conservative: avoid false positives (questions/requests).
//   - Silent: never produces a chat acknowledgement; UI LED/count will reflect changes.
//   - Conflict-aware: uses ProposePendingRememberFact (may route to CONFLICTS).
func maybeAutoProposePendingFromUserInput(cfg Config, db *sql.DB, input string, now time.Time) (*RememberOutcome, error) {
	if db == nil {
		return nil, nil
	}
	text := strings.TrimSpace(input)
	if text == "" {
		return nil, nil
	}
	// Skip commands and explicit remember/forget lines (handled elsewhere).
	if strings.HasPrefix(text, "/") {
		return nil, nil
	}
	if _, _, ok := parseAutoFactsIntent(text); ok {
		return nil, nil
	}
	// Heuristic: only consider high-signal self-statements.
	if !looksLikeSelfStatement(text) {
		return nil, nil
	}
	// Avoid capturing vague moods like "我很累"; require some "attribute" markers.
	if !(strings.Contains(text, "是") || strings.Contains(text, "叫") || strings.Contains(text, "生日") || strings.Contains(text, "最喜欢") || strings.Contains(text, "喜欢")) {
		return nil, nil
	}
	// Keep this conservative to reduce spam, but allow reasonably long natural sentences.
	if len([]rune(text)) < 4 || len([]rune(text)) > 140 {
		return nil, nil
	}

	fact := strings.TrimSpace(strings.TrimRight(text, "。.!！"))
	when := now
	loc := cfg.Location
	if loc != nil {
		when = when.In(loc)
	}
	sourceKey := when.Format("2006-01-02")
	// Best-effort, silent. No user-visible acknowledgement.
	st, err := ProposePendingRememberFact(cfg, db, fact, "realtime_implicit", sourceKey, when)
	return st, err
}

// sanitizeAssistantText removes accidental internal / operational markers if the model
// echoes them. This prevents UI pollution and prevents these markers from entering
// recent_raw injection.
func sanitizeAssistantText(s string) string {
	if s == "" {
		return s
	}

	// 1) Strip misleading memory-claim phrases.
	// The model sometimes replies with "已记住：..."; we must not surface that.
	// Memory/facts are handled silently by the system.
	trimmed := strings.TrimSpace(s)
	for _, p := range []string{"已记住：", "已记住:", "已记录：", "已记录:", "我已记住：", "我已记住:", "我会记住：", "我会记住:", "我已经记住：", "我已经记住:"} {
		if strings.HasPrefix(trimmed, p) {
			s = strings.TrimSpace(strings.TrimPrefix(trimmed, p))
			break
		}
	}

	// 2) Strip meta-disclaimer parentheticals like "（遵循身份契约...）".
	// These add noise and can confuse FACTS testing.
	if strings.Contains(s, "身份契约") || strings.Contains(s, "指代规则") {
		s = stripNoisyParentheticals(s)
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			out = append(out, ln)
			continue
		}
		// Drop obvious internal acks that should never be user-visible.
		if strings.HasPrefix(t, "[ok]") || strings.HasPrefix(t, "[noop]") || strings.HasPrefix(t, "[conflict]") || strings.HasPrefix(t, "[error]") {
			if strings.Contains(t, "FACTS") || strings.Contains(t, "待确认事实") || strings.Contains(t, "长期事实") || strings.Contains(t, "PENDING") || strings.Contains(t, "CONFLICTS") {
				continue
			}
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// stripNoisyParentheticals removes any parenthetical segments that contain
// identity-contract / meta-policy wording (e.g. "身份契约", "指代规则").
// It is intentionally conservative: only removes parentheses that contain those keywords.
func stripNoisyParentheticals(s string) string {
	keywords := []string{"身份契约", "指代规则"}
	removeOne := func(open, close string) (string, bool) {
		i := strings.Index(s, open)
		if i < 0 {
			return s, false
		}
		j := strings.Index(s[i+len(open):], close)
		if j < 0 {
			return s, false
		}
		j = i + len(open) + j
		seg := s[i+len(open) : j]
		for _, kw := range keywords {
			if strings.Contains(seg, kw) {
				// remove [i, j+len(close))
				return strings.TrimSpace(s[:i] + s[j+len(close):]), true
			}
		}
		return s, false
	}

	// Remove multiple segments if present.
	for iter := 0; iter < 8; iter++ {
		if out, ok := removeOne("（", "）"); ok {
			s = out
			continue
		}
		if out, ok := removeOne("(", ")"); ok {
			s = out
			continue
		}
		break
	}
	return strings.TrimSpace(s)
}
