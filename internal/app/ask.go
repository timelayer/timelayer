package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

/*
========================
Public API
========================
*/

// Ask answers a question based on user's historical memory.
// It relies on LLM to explicitly declare whether the answer
// is supported by memory (supported: true/false).
func Ask(db *sql.DB, cfg Config, input string) (string, error) {
	question, showRefs := parseAskArgs(input)

	// 1️⃣ semantic search (pure retrieval, no semantics)
	hits, err := SearchWithScore(db, cfg, question)
	if err != nil {
		return "", err
	}

	// 2️⃣ build memory context (TopK only)
	var ctx strings.Builder
	ctx.WriteString("以下是我在你过去记录中找到的相关内容：\n\n")

	for i, h := range hits {
		if i >= cfg.SearchTopK {
			break
		}
		ctx.WriteString(fmt.Sprintf(
			"- [%s %s | score %.2f]\n%s\n\n",
			h.Date,
			h.Type,
			h.Score,
			h.Text,
		))
	}

	// 3️⃣ compose prompt (STRUCTURED output)
	prompt := buildAskPrompt(ctx.String(), question)

	// 4️⃣ call LLM
	raw, err := callLLMNonStream(cfg, prompt)
	if err != nil {
		return "", err
	}

	// 5️⃣ parse structured answer
	type askResult struct {
		Supported bool   `json:"supported"`
		Answer    string `json:"answer"`
	}

	var ar askResult
	if err := json.Unmarshal([]byte(raw), &ar); err != nil {
		// ⛑️ fallback: model didn't follow protocol
		Speak(raw)
		return raw, nil
	}

	// 6️⃣ build final output
	var out strings.Builder
	out.WriteString(ar.Answer)

	// ✅ only attach references when explicitly supported
	if ar.Supported && len(hits) > 0 {
		out.WriteString("\n\n——\n")
		out.WriteString(formatTopReference(hits[0]))

		if showRefs {
			out.WriteString("\n\n附录 · 相关记录（最多 10 条）：\n")
			max := min(10, len(hits))
			for i := 0; i < max; i++ {
				out.WriteString(formatRefLine(i+1, hits[i]))
				out.WriteString("\n")
			}
		}
	}

	// TTS only reads core answer
	Speak(ar.Answer)
	return out.String(), nil
}

/*
========================
Argument Parser
========================
*/

func parseAskArgs(input string) (question string, showRefs bool) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", false
	}

	var q []string
	for _, p := range parts {
		if p == "--refs" {
			showRefs = true
		} else {
			q = append(q, p)
		}
	}
	return strings.Join(q, " "), showRefs
}

/*
========================
Prompt Builder (STRUCTURED)
========================
*/

func buildAskPrompt(memoryContext, question string) string {
	return fmt.Sprintf(`
你是“基于用户自身长期记忆”的智能助理，而不是百科或搜索引擎。

【重要原则】
- 你只能基于“用户自己的历史记录”来回答
- 如果历史记录不足以支撑结论，必须明确说明
- 不要假装知道用户未记录的事实
- 不要扩展、推断、脑补未出现的信息

【用户的历史记录】
%s

【用户当前的问题】
%s

【你的任务】
请严格按照以下 JSON 格式输出结果（只输出 JSON，不要输出任何额外文字）：

{
  "supported": true | false,
  "answer": "你的自然语言回答"
}

规则：
- supported = true：表示历史记录中存在明确证据支撑回答
- supported = false：表示历史记录不足以支撑回答
- 当 supported = false 时：
  - answer 必须用自然、友好、人类对话方式说明原因
  - 可以解释为“当前记忆中没有相关记录”
  - 语气应温和、克制、符合长期对话助理的风格
  - 不要使用生硬或像系统报错一样的表述

`, memoryContext, question)
}

/*
========================
Reference Formatting
========================
*/

// Top-1 reference
func formatTopReference(h SearchHit) string {
	if h.Type == "fact" {
		return fmt.Sprintf(
			"参考事实：%s",
			strings.TrimSpace(h.Text),
		)
	}

	return fmt.Sprintf(
		"参考：你在 %s 的 %s 记录中提到：%s",
		h.Date,
		h.Type,
		firstLine(h.Text),
	)
}

// Appendix reference line
func formatRefLine(idx int, h SearchHit) string {
	if h.Type == "fact" {
		return fmt.Sprintf(
			"%d. [%.2f] FACT · %s",
			idx,
			h.Score,
			strings.TrimSpace(h.Text),
		)
	}

	return fmt.Sprintf(
		"%d. [%.2f] %s %s · %s",
		idx,
		h.Score,
		h.Date,
		h.Type,
		firstLine(h.Text),
	)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimPrefix(s[:i], "- ")
	}
	return strings.TrimPrefix(s, "- ")
}
