package app

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

/*
================================================
DebugChat
- 构造与 Chat() 完全一致的 system prompt
- 不调用模型
- 输出「可审计」的完整证据链
================================================
*/

// DebugChat：直接打印 Debug 结果
func DebugChat(cfg Config, db *sql.DB, input string) {
	fmt.Println(DebugChatText(cfg, db, input))
}

// DebugChatText：返回 Debug 文本
func DebugChatText(cfg Config, db *sql.DB, input string) string {
	now := time.Now().In(cfg.Location)
	date := now.Format("2006-01-02")

	// 与 Chat 使用完全一致的上下文
	blocks := BuildChatContext(cfg, db, date, input)

	var system strings.Builder

	// =========================
	// 1️⃣ 系统事实（权威）
	// =========================
	system.WriteString("【系统事实（权威）】\n")
	system.WriteString(fmt.Sprintf(
		"日期：%s\n时间：%s\n星期：%s\n时区：%s\n\n",
		now.Format("2006-01-02"),
		now.Format("15:04:05"),
		now.Weekday().String(),
		now.Location().String(),
	))
	system.WriteString(
		"以上时间信息来自系统，是准确且可信的事实。\n" +
			"涉及日期、时间、星期的问题，请直接基于这些事实回答。\n\n",
	)

	// =========================
	// 2️⃣ Search 命中明细（证据层）
	// =========================
	system.WriteString("【Search 命中明细（Embedding 证据）】\n")

	hits, err := SearchWithScore(db, cfg, input)
	if err != nil || len(hits) == 0 {
		system.WriteString("(无 search 命中)\n\n")
	} else {
		for i, h := range hits {
			system.WriteString(fmt.Sprintf(
				"%d) type=%s | score=%.4f\n",
				i+1, h.Type, h.Score,
			))

			if strings.TrimSpace(h.Text) != "" {
				system.WriteString("   内容：")
				system.WriteString(h.Text)
				system.WriteString("\n")
			} else {
				system.WriteString("   内容：(无可读文本，可能为结构型 summary)\n")
			}
			system.WriteString("\n")
		}
	}

	// =========================
	// 3️⃣ 本次可能使用的事实（fact）
	// =========================
	system.WriteString("【本次可能使用的事实（Fact Evidence）】\n")

	factUsed := false
	for _, h := range hits {
		if h.Type == "fact" && strings.TrimSpace(h.Text) != "" {
			system.WriteString("- ")
			system.WriteString(h.Text)
			system.WriteString("\n")
			system.WriteString("  来源：显式事实（/remember）\n")
			system.WriteString("  证据：embedding search 命中\n\n")
			factUsed = true
		}
	}
	if !factUsed {
		system.WriteString("(无 fact 通过 search 命中)\n\n")
	}

	// =========================
	// 4️⃣ Prompt Blocks（最终注入模型）
	// =========================
	system.WriteString("【最终 Prompt Blocks（直接注入模型）】\n\n")

	for _, b := range blocks {
		system.WriteString("---- Prompt Block ----\n")
		system.WriteString("Role: " + b.Role + "\n")
		system.WriteString("Source: " + b.Source + "\n")
		system.WriteString("Memory Level: " + memoryLevel(b.Source) + "\n")

		if isAuthoritative(b.Source) {
			system.WriteString("Authority: HIGH（权威事实，不应被质疑）\n")
		} else {
			system.WriteString("Authority: NORMAL\n")
		}

		system.WriteString("\n")
		system.WriteString(b.Content)
		system.WriteString("\n\n")
	}

	// =========================
	// 5️⃣ 输出包装
	// =========================
	var out strings.Builder
	out.WriteString("========== DEBUG CHAT ==========\n")
	out.WriteString("【User Input】\n")
	out.WriteString(input)
	out.WriteString("\n\n")
	out.WriteString("【System Prompt（发送给模型的完整内容）】\n")
	out.WriteString(system.String())
	out.WriteString("================================\n")

	return out.String()
}

/*
========================
辅助：记忆层级 & 权威性
========================
*/

func memoryLevel(source string) string {
	switch source {
	case "remembered_fact":
		return "EXPLICIT_FACT (user confirmed)"
	case "search_hit":
		return "LONG_TERM (embedding search)"
	case "daily_summary":
		return "ABSTRACT (daily summary)"
	case "recent_raw":
		return "SHORT_TERM (recent dialog)"
	default:
		return "UNKNOWN"
	}
}

func isAuthoritative(source string) bool {
	return source == "remembered_fact"
}
