package app

import (
	"database/sql"
	"fmt"
	"strings"
)

/*
================================================
Summary Guard
- Fact ↔ Summary 权威冲突检测
- Daily / Weekly 输出自检
- 只报警，不修正
================================================
*/

type SummaryWarning struct {
	Level   string // WARN / ERROR
	Type    string // FACT_CONFLICT / LINT
	Message string
}

// ========================
// Public Entry
// ========================

func RunSummaryGuards(
	db *sql.DB,
	summaryType string, // daily / weekly / monthly
	summaryJSON string,
) []SummaryWarning {

	var warnings []SummaryWarning

	claims := extractSummaryClaims(summaryJSON)

	// 1️⃣ Fact 冲突检测（只对 daily / weekly）
	if summaryType == "daily" || summaryType == "weekly" {
		ws := detectFactConflicts(db, claims)
		warnings = append(warnings, ws...)
	}

	// 2️⃣ Summary 自检（lint）
	ws := lintSummary(summaryType, summaryJSON)
	warnings = append(warnings, ws...)

	return warnings
}

// ========================
// Fact Conflict Detection
// ========================

func detectFactConflicts(db *sql.DB, claims []string) []SummaryWarning {
	var warnings []SummaryWarning

	rows, err := db.Query(`
		SELECT fact
		FROM user_facts
		WHERE is_active=1
	`)
	if err != nil {
		return warnings
	}
	defer rows.Close()

	var facts []string
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil {
			facts = append(facts, f)
		}
	}

	for _, claim := range claims {
		subject := extractFactSubject(claim)
		if subject == "" {
			continue
		}

		for _, fact := range facts {
			if extractFactSubject(fact) != subject {
				continue
			}

			// ⚠️ summary 提到同一 subject，但未包含完整 fact
			if !strings.Contains(claim, fact) {
				warnings = append(warnings, SummaryWarning{
					Level: "WARN",
					Type:  "FACT_CONFLICT",
					Message: fmt.Sprintf(
						"Summary claim may conflict with authoritative fact.\n- Fact: %s\n- Summary: %s",
						fact,
						claim,
					),
				})
			}
		}
	}

	return warnings
}

// ========================
// Summary Claim Extraction
// ========================

// 非 NLP，仅抽取“可能是事实陈述”的句子
func extractSummaryClaims(text string) []string {
	var claims []string

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}

		// 极简规则：包含“是 / 为 / 担任 / 属于”
		if strings.Contains(l, "是") ||
			strings.Contains(l, "为") ||
			strings.Contains(l, "担任") ||
			strings.Contains(l, "属于") {
			claims = append(claims, l)
		}
	}

	return claims
}

// ========================
// Lint Rules
// ========================

func lintSummary(summaryType, text string) []SummaryWarning {
	var warnings []SummaryWarning

	forbidden := []string{
		"可能", "似乎", "推测", "大概",
		"建议", "应该", "值得",
	}

	for _, k := range forbidden {
		if strings.Contains(text, k) {
			warnings = append(warnings, SummaryWarning{
				Level:   "WARN",
				Type:    "LINT",
				Message: fmt.Sprintf("Summary contains speculative or advisory word: %q", k),
			})
		}
	}

	// Weekly / Monthly 特殊规则
	if summaryType != "daily" {
		if strings.Contains(text, "今天") || strings.Contains(text, "昨日") {
			warnings = append(warnings, SummaryWarning{
				Level:   "WARN",
				Type:    "LINT",
				Message: "Non-daily summary references specific day-level events",
			})
		}
	}

	return warnings
}
