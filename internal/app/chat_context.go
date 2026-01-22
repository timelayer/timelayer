package app

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

/*
PromptBlock 表示一段被注入到 LLM Prompt 中的上下文块。
它描述的是「Prompt 组成」，而不是「记忆层级」。
*/
type PromptBlock struct {
	Role    string // system | user | assistant
	Source  string // daily_summary | search_hit | recent_raw | remembered_fact
	Content string
}

/*
memoryEvidence 是 PromptBlock 的裁决前形态。
所有记忆必须先进入 evidence，再统一裁决后进入 prompt。
*/
type memoryEvidence struct {
	Role     string
	Source   string
	Content  string
	Priority int // 越大越不可被丢弃
}

// 构建 chat 上下文（被 Chat / DebugChat 行为调用）
// 注意：这里只负责“Prompt 组装”，不注入当前 user input
func BuildChatContext(
	cfg Config,
	db *sql.DB,
	date string,
	userQuestion string, // 保留参数，仅用于 search
) []PromptBlock {

	var evidences []memoryEvidence

	// ------------------------------------------------------------
	// 0️⃣ 显式长期事实（/remember）——最高优先级（硬规则）
	// ------------------------------------------------------------

	rememberedSet := map[string]struct{}{}

	if facts, err := loadActiveUserFacts(db, 50); err == nil && len(facts) > 0 {
		var b strings.Builder
		b.WriteString("以下是用户明确要求我长期记住的事实（高优先级、确定，不要质疑）：\n")

		for _, f := range facts {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			rememberedSet[f] = struct{}{}
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}

		if b.Len() > 0 {
			evidences = append(evidences, memoryEvidence{
				Role:     "assistant",
				Source:   "remembered_fact",
				Content:  b.String(),
				Priority: 1000, // 🔒 写死：永不被裁掉
			})
		}
	}

	// ------------------------------------------------------------
	// 1️⃣ 今日 daily summary（自动抽象，低权威）
	//     - 过滤已被 /remember 确认的 user_facts_explicit
	// ------------------------------------------------------------

	if daily := loadDailySummary(cfg, date); daily != "" {

		var obj map[string]any
		if err := json.Unmarshal([]byte(daily), &obj); err == nil {

			// 🔑 裁决：过滤已确认的事实（支持 string / object 两种形态）
			if v, ok := obj["user_facts_explicit"]; ok {
				if arr, ok := v.([]any); ok {
					var filtered []any
					for _, it := range arr {
						switch x := it.(type) {
						case string:
							s := strings.TrimSpace(x)
							if s == "" {
								continue
							}
							if _, exists := rememberedSet[s]; exists {
								continue
							}
							filtered = append(filtered, s)
						case map[string]any:
							fact := ""
							if f, ok := x["fact"].(string); ok {
								fact = f
							} else if f, ok := x["content"].(string); ok {
								fact = f
							}
							s := strings.TrimSpace(fact)
							if s == "" {
								continue
							}
							if _, exists := rememberedSet[s]; exists {
								continue
							}
							filtered = append(filtered, x)
						default:
							// ignore unknown shapes
						}
					}

					if len(filtered) > 0 {
						obj["user_facts_explicit"] = filtered
					} else {
						delete(obj, "user_facts_explicit")
					}
				}
			}

			if b, err := json.MarshalIndent(obj, "", "  "); err == nil {
				daily = string(b)
			}
		}

		evidences = append(evidences, memoryEvidence{
			Role:     "assistant",
			Source:   "daily_summary",
			Content:  "这是今天的对话摘要（包含自动推断内容，未必完全准确）：\n" + daily,
			Priority: 600,
		})
	}

	// ------------------------------------------------------------
	// 2️⃣ 相似历史（embedding 命中）
	// ------------------------------------------------------------

	hits, err := SearchWithScore(db, cfg, userQuestion)
	if err == nil && len(hits) > 0 {
		var b strings.Builder
		b.WriteString("以下内容是通过语义相似度检索得到，可能与当前问题相关，但未必完全准确：\n")
		included := 0

		max := min(cfg.SearchTopK, len(hits))
		for i := 0; i < max; i++ {
			h := hits[i]
			if h.Type == "daily" && h.Date == date {
				continue
			}
			// ✅ 去重：如果命中内容与已 /remember 的事实完全一致，就不重复注入
			if _, exists := rememberedSet[strings.TrimSpace(h.Text)]; exists {
				continue
			}
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(h.Text))
			b.WriteString("\n")
			included++
		}

		if included > 0 {
			evidences = append(evidences, memoryEvidence{
				Role:     "assistant",
				Source:   "search_hit",
				Content:  b.String(),
				Priority: 400,
			})
		}
	}

	// ------------------------------------------------------------
	// 3️⃣ 最近 raw 对话（短期上下文）
	// ------------------------------------------------------------

	maxLines := cfg.RecentMaxLines
	if maxLines <= 0 {
		maxLines = 20
	}
	if recent := loadRecentRaw(cfg, date, maxLines); recent != "" {
		evidences = append(evidences, memoryEvidence{
			Role:     "assistant",
			Source:   "recent_raw",
			Content:  "以下是最近的原始对话记录：\n" + recent,
			Priority: 200,
		})
	}

	// 🔒 统一裁决出口（不可绕过）
	return resolvePromptBlocks(evidences)
}

// ------------------------------------------------------------
// 裁决：唯一出口（✅ 零破坏式根治点）
// - 不改外部结构、不删 Role
// - 但在“注入 prompt 前”强制降权 + 清洗人格自述
// ------------------------------------------------------------

func resolvePromptBlocks(evs []memoryEvidence) []PromptBlock {
	// 当前只做两件事：
	// 1) 保证 remembered_fact 永远最优先
	// 2) 强制上下文降权为“参考信息”，剥夺人格自述能力（根治）
	var facts []PromptBlock
	type otherBlock struct {
		pb   PromptBlock
		prio int
		idx  int
	}
	var others []otherBlock
	idx := 0

	for _, e := range evs {
		content := sanitizeForContext(e.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}

		pb := PromptBlock{
			Role:   "assistant", // ✅ 强制降权：上下文永远不能拥有 system/user 发言权
			Source: e.Source,
			// ✅ 强制加“参考信息”包装，避免被当成“模型自述”
			Content: content,
		}

		if e.Source == "remembered_fact" {
			facts = append(facts, pb)
		} else {
			others = append(others, otherBlock{pb: pb, prio: e.Priority, idx: idx})
		}
		idx++
	}

	// ✅ 让注入顺序更稳定：按 Priority 降序（同优先级保持原始顺序）
	sort.SliceStable(others, func(i, j int) bool {
		if others[i].prio != others[j].prio {
			return others[i].prio > others[j].prio
		}
		return others[i].idx < others[j].idx
	})

	out := make([]PromptBlock, 0, len(facts)+len(others))
	out = append(out, facts...)
	for _, ob := range others {
		out = append(out, ob.pb)
	}
	return out
}

// ------------------------------------------------------------
// 人格/自述防火墙：把“我是通义千问/小天/AI助手…”这类句子从上下文中剔除
// 同时把内容统一包装成【参考信息】
// ------------------------------------------------------------

func sanitizeForContext(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	lines := strings.Split(s, "\n")
	var kept []string

	containsAny := func(hay string, subs []string) bool {
		for _, sub := range subs {
			if sub == "" {
				continue
			}
			if strings.Contains(hay, sub) {
				return true
			}
		}
		return false
	}

	looksLikeAssistantSelfIntro := func(line string) bool {
		l := strings.TrimSpace(line)
		if l == "" {
			return false
		}
		low := strings.ToLower(l)

		// English-ish patterns
		if strings.HasPrefix(low, "i am") || strings.HasPrefix(low, "i'm") || strings.Contains(low, "as an ai") {
			if containsAny(low, []string{"chatgpt", "openai", "ai assistant", "language model"}) {
				return true
			}
		}
		if containsAny(low, []string{"chatgpt", "openai", "language model", "ai assistant"}) &&
			(containsAny(low, []string{"i am", "i'm"}) || strings.Contains(low, "as an")) {
			return true
		}

		// Chinese patterns: only remove when it clearly declares assistant identity
		// (avoid deleting user sentences like “我是程序员”)
		cnMarkers := []string{"AI助手", "语言模型", "通义", "通义千问", "Qwen", "阿里巴巴", "ChatGPT", "OpenAI", "小天"}
		hasMarker := containsAny(l, cnMarkers) || containsAny(low, []string{"qwen"})
		if hasMarker {
			if strings.Contains(l, "我是") || strings.Contains(l, "作为一个") || strings.Contains(l, "作为") {
				return true
			}
			// 也拦“我可以协助你…”这类典型自述
			if strings.Contains(l, "我主要可以") || strings.Contains(l, "我可以") {
				return true
			}
		}

		return false
	}

	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}

		// 🚫 防止人格串权：仅剔除“助手自我介绍/身份声明”类语句
		if looksLikeAssistantSelfIntro(l) {
			continue
		}

		kept = append(kept, l)
	}

	if len(kept) == 0 {
		return ""
	}

	// ✅ 统一降权声明：它是资料，不是“谁说的话”
	return "【参考信息】\n" + strings.Join(kept, "\n")
}

// ------------------------------------------------------------
// helpers
// ------------------------------------------------------------

func loadDailySummary(cfg Config, date string) string {
	path := filepath.Join(cfg.LogDir, date+".daily.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func loadRecentRaw(cfg Config, date string, maxLines int) string {
	path := filepath.Join(cfg.LogDir, date+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(b), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	var out []string

	// 单条消息最长字符数（避免把很长的 assistant 回复塞爆 prompt）
	// 需要更长可以调大；保持保守能显著降低上下文污染与延迟。
	const maxCharsPerMsg = 900

	format := func(prefix string, content string, hint string) string {
		c := strings.TrimSpace(content)
		if c == "" {
			return ""
		}

		// 统一一下换行与尾部空白
		c = strings.ReplaceAll(c, "\r\n", "\n")
		c = strings.ReplaceAll(c, "\r", "\n")
		c = strings.TrimSpace(c)

		// 截断超长内容
		if len([]rune(c)) > maxCharsPerMsg {
			r := []rune(c)
			c = string(r[:maxCharsPerMsg]) + " …（已截断）"
		}

		// 多行内容：首行加 prefix，后续行缩进，避免“我/你”漂移
		lines := strings.Split(c, "\n")
		var b strings.Builder
		b.WriteString(prefix)
		b.WriteString(lines[0])
		if hint != "" {
			b.WriteString(hint)
		}
		for i := 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if l == "" {
				continue
			}
			b.WriteString("\n  ")
			b.WriteString(l)
		}
		return b.String()
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var m struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Kind    string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		// Never inject internal/operational logs into recent_raw.
		if strings.TrimSpace(m.Kind) == "op" {
			continue
		}

		switch m.Role {
		case "user":
			if s := format("用户：", m.Content, ""); s != "" {
				out = append(out, s)
			}
		case "assistant":
			// Drop accidental internal markers that could pollute future turns.
			trim := strings.TrimSpace(m.Content)
			if strings.HasPrefix(trim, "[ok]") || strings.HasPrefix(trim, "[noop]") || strings.HasPrefix(trim, "[conflict]") || strings.HasPrefix(trim, "[error]") {
				if strings.Contains(trim, "FACTS") || strings.Contains(trim, "待确认事实") || strings.Contains(trim, "PENDING") || strings.Contains(trim, "CONFLICTS") {
					continue
				}
			}
			// ✅ 关键：把 assistant 的历史回复也注入，但明确降权为“仅供语境”
			// 这能显著提升连续追问/承接能力，同时降低把旧回复当事实的风险。
			if s := format("助手：", m.Content, "（仅供语境，不保证正确）"); s != "" {
				out = append(out, s)
			}
		default:
			// ignore
		}
	}

	if len(out) == 0 {
		return ""
	}

	return strings.Join(out, "\n")
}
