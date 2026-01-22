package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Chat = 对话行为入口（CLI）
// 仍保持原行为：边输出边写日志，不返回文本
func Chat(lw *LogWriter, cfg Config, db *sql.DB, input string) error {
	_, err := ChatOnce(lw, cfg, db, input, true, nil)
	return err
}

// ChatOnce keeps backward compatibility (no ctx) and returns the full answer.
func ChatOnce(
	lw *LogWriter,
	cfg Config,
	db *sql.DB,
	input string,
	printToStdout bool,
	onDelta func(string),
) (string, error) {
	return ChatOnceWithContext(context.Background(), lw, cfg, db, input, printToStdout, onDelta)
}

// ChatOnceWithContext runs one chat turn, writes logs, and returns the full answer.
// - ctx is used to cancel upstream streaming when web client disconnects.
func ChatOnceWithContext(
	ctx context.Context,
	lw *LogWriter,
	cfg Config,
	db *sql.DB,
	input string,
	printToStdout bool,
	onDelta func(string),
) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", nil
	}

	now := time.Now().In(cfg.Location)
	origInput := input
	effectiveInput := input
	skipImplicit := false

	// ------------------------------------------------------------
	// ✅ Auto facts intent (very explicit): "记住：..." / "忘记：..."
	// New UX goal:
	//   - The facts action is handled silently in the background
	//   - BUT the assistant should still reply normally (no empty interaction)
	//     by chatting over the underlying fact text (without the prefix).
	// ------------------------------------------------------------
	if action, fact, ok := parseAutoFactsIntent(input); ok {
		_ = lw.WriteRecord(map[string]string{
			"role":    "user",
			"content": origInput,
			"kind":    "op",
		})
		when := now
		sourceKey := when.Format("2006-01-02")
		var resp string

		switch action {
		case "remember":
			if strings.TrimSpace(fact) == "" {
				resp = "usage: 记住：<fact>"
				_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": resp, "kind": "op"})
				if printToStdout {
					fmt.Println(resp)
				}
				return resp, nil
			}
			// Background: propose into FACTS (pending/conflict/noop). No chat acknowledgement.
			_, err := ProposePendingRememberFact(cfg, db, fact, "remember_auto", sourceKey, when)
			if err != nil {
				resp = "[warn] pending facts ingest failed: " + err.Error()
				_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": resp, "kind": "op"})
			}
			effectiveInput = strings.TrimSpace(fact)
			skipImplicit = true
			// Also log the "real" user meaning (so recent_raw continuity is good).
			_ = lw.WriteRecord(map[string]string{"role": "user", "content": effectiveInput})

		case "forget":
			if strings.TrimSpace(fact) == "" {
				resp = "usage: 忘记：<fact>"
				_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": resp, "kind": "op"})
				if printToStdout {
					fmt.Println(resp)
				}
				return resp, nil
			}
			if err := RetractFact(cfg, db, fact, "forget_auto", sourceKey, when); err != nil {
				// Don't lie to the user. Keep it short and non-technical.
				_ = lw.WriteRecord(map[string]string{
					"role":    "assistant",
					"content": "[warn] forget failed: " + err.Error(),
					"kind":    "op",
				})
				resp = "抱歉，我这边没能完成这个操作，请稍后再试一次。"
			} else {
				// Provide a tiny normal reply without mentioning internal systems.
				resp = "好的。"
			}
			resp = sanitizeAssistantText(resp)
			_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": resp})
			if printToStdout {
				fmt.Println(resp)
			}
			return resp, nil
		}
	}

	// write user (normal chat)
	// (If it was an explicit remember intent, we already logged the cleaned meaning above.)
	if !(skipImplicit && strings.TrimSpace(effectiveInput) != "" && origInput != effectiveInput) {
		_ = lw.WriteRecord(map[string]string{
			"role":    "user",
			"content": effectiveInput,
		})
	}

	// ------------------------------------------------------------
	// ✅ Implicit self-fact -> silently propose into FACTS → PENDING
	// (no chat acknowledgement; UI only shows LED/count)
	// ------------------------------------------------------------
	if !skipImplicit {
		if _, err := maybeAutoProposePendingFromUserInput(cfg, db, effectiveInput, now); err != nil {
			// Keep UX quiet; but log the failure for operators.
			_ = lw.WriteRecord(map[string]string{
				"role":    "assistant",
				"content": "[warn] pending facts ingest failed: " + err.Error(),
				"kind":    "op",
			})
		}
	}

	// ✅ system + context messages（把记忆/检索从 system 降权出来）
	system, ctxMsgs := buildSystemPrompt(cfg, db, now, effectiveInput)

	// ✅ 小包装：降低中文“我/你”歧义
	modelInput := "【用户原话】\n" + effectiveInput

	// stream
	if printToStdout {
		ans := streamChatWithContextCLI(cfg, system, ctxMsgs, modelInput)
		ans = sanitizeAssistantText(ans)
		_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": ans})
		return ans, nil
	}

	ans, err := streamChatWithContextCtx(ctx, cfg, system, ctxMsgs, modelInput, onDelta)
	if err != nil {
		return ans, err
	}

	ans = sanitizeAssistantText(ans)
	_ = lw.WriteRecord(map[string]string{"role": "assistant", "content": ans})

	return ans, nil
}
