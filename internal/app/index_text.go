package app

import (
	"encoding/json"
	"strings"
)

func extractIndexText(summaryJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(summaryJSON), &m); err != nil {
		return summaryJSON
	}

	var parts []string
	seen := make(map[string]struct{}) // ⭐ 去重表

	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {

		case string:
			s := strings.TrimSpace(x)
			// ✅ 降噪规则：太短 / 太长的文本不进入 embedding
			if runeLen(s) >= 2 && runeLen(s) <= 200 {
				// ✅ 去重：同一句只进一次
				if _, ok := seen[s]; !ok {
					seen[s] = struct{}{}
					parts = append(parts, s)
				}
			}

		case []any:
			for _, it := range x {
				collect(it)
			}

		case map[string]any:
			for _, vv := range x {
				collect(vv)
			}
		}
	}

	// 只从“记忆友好型字段”中抽取
	for _, k := range []string{
		"tags",
		"themes",
		"topics",
		"projects",
		"decisions",
		"patterns",
		"highlights",
		"lowlights",
		"user_facts_explicit",
		"next_week_focus",
		"next_month_bets",
	} {
		if v, ok := m[k]; ok {
			collect(v)
		}
	}

	text := strings.Join(parts, "\n")
	text = strings.TrimSpace(text)

	// fallback：保证 embedding 永远有内容
	if text == "" {
		return summaryJSON
	}
	return text
}
