package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rivo/uniseg"
)

/*
========================
SSE 数据结构
========================
*/

type SSEChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

/*
========================
对外接口（无上下文）
========================
*/

func streamChat(cfg Config, question string) string {
	return streamChatWithContextCLI(cfg, "", nil, question)
}

/*
========================
CLI 版本（边打字边输出）
========================
*/

type typewriterState struct {
	inCodeBlock bool
	backticks   int // 连续反引号计数，用于处理跨 chunk 的 ```
}

func streamChatWithContextCLI(
	cfg Config,
	systemPrompt string,
	contextMessages []map[string]string,
	userQuestion string,
) string {
	st := &typewriterState{}

	out, err := streamChatWithContextCtx(
		context.Background(),
		cfg,
		systemPrompt,
		contextMessages,
		userQuestion,
		func(delta string) {
			printWithTypewriter(delta, st)
		},
	)

	if err != nil {
		fmt.Printf("\n(stream error) %v\n", err)
	}
	fmt.Print("\n")
	return out
}

/*
========================
核心流式请求（V2.1）
========================
*/

// streamChatWithContext keeps backward compatibility (no ctx).
func streamChatWithContext(
	cfg Config,
	systemPrompt string,
	contextMessages []map[string]string,
	userQuestion string,
	onDelta func(string),
) (string, error) {
	return streamChatWithContextCtx(context.Background(), cfg, systemPrompt, contextMessages, userQuestion, onDelta)
}

// streamChatWithContextCtx supports cancellation (web client disconnect).
func streamChatWithContextCtx(
	ctx context.Context,
	cfg Config,
	systemPrompt string,
	contextMessages []map[string]string,
	userQuestion string,
	onDelta func(string),
) (string, error) {

	/*
		========================
		1️⃣ 构造 messages
		========================
	*/

	messages := []map[string]string{}

	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	for _, m := range contextMessages {
		if m["role"] != "" && m["content"] != "" {
			messages = append(messages, m)
		}
	}

	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userQuestion,
	})

	/*
		========================
		2️⃣ 自动判断是否启用 thinking
		========================
	*/

	enableThinking := shouldEnableThinkingV2(userQuestion)

	/*
		========================
		3️⃣ 构造 payload
		========================
	*/

	payload := map[string]any{
		"model":           cfg.ChatModel,
		"messages":        messages,
		"stream":          true,
		"enable_thinking": enableThinking, // llama.cpp server 目前不会在运行时消费该字段
		// thinking 行为在服务端启动阶段已由 chat template 固定。
		// 保留该参数用于上游逻辑判断及未来 server 行为对齐。
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", cfg.ChatURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: cfg.HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		bb, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("http error %d: %s", resp.StatusCode, strings.TrimSpace(string(bb)))
	}

	/*
		========================
		4️⃣ 读取 SSE 流
		========================
	*/

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)

	var full strings.Builder

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return full.String(), ctx.Err()
		default:
		}

		line := scanner.Text()
		line = strings.TrimRight(line, "\r") // ✅ 兼容 CRLF

		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var chunk SSEChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}

		full.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}

	if err := scanner.Err(); err != nil {
		return full.String(), err
	}

	return full.String(), nil
}

/*
========================
打字机输出
========================
*/

func printWithTypewriter(text string, st *typewriterState) {
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		ch := gr.Str()
		fmt.Print(ch)

		// ✅ 精确处理跨 chunk 的 ```
		if ch == "`" {
			st.backticks++
			if st.backticks == 3 {
				st.inCodeBlock = !st.inCodeBlock
				st.backticks = 0
			}
		} else {
			st.backticks = 0
		}

		if !st.inCodeBlock {
			switch ch {
			case "。", "！", "？":
				fmt.Print("\n")
			}
		}
	}
}

/*
========================
Thinking 自动判定（V2.1）
========================
*/

func shouldEnableThinkingV2(q string) bool {
	q = strings.TrimSpace(q)

	// ⭐ 0️⃣ 短寒暄 / 问候语 → 强制关闭
	if isShortGreeting(q) {
		return false
	}

	score := 0
	score += structureScore(q)
	score += abstractionScore(q)
	score += nonTemplateScore(q)
	score += lengthScore(q)

	// ⭐ 阈值：>=3 才开启 thinking
	return score >= 3
}

/*
========================
寒暄兜底（关键）
========================
*/

func isShortGreeting(q string) bool {
	if len([]rune(q)) <= 4 {
		switch strings.ToLower(q) {
		case "你好", "您好", "在吗", "hi", "hello", "hey", "？", "?":
			return true
		}
	}
	return false
}

/*
========================
评分模块
========================
*/

func structureScore(q string) int {
	s := 0

	if strings.Count(q, "?")+strings.Count(q, "？") >= 2 {
		s += 2
	}
	if strings.Contains(q, "如果") ||
		strings.Contains(q, "假设") ||
		strings.Contains(q, "在") && strings.Contains(q, "情况下") ||
		strings.Contains(strings.ToLower(q), "if ") {
		s += 2
	}
	if strings.Contains(q, "并且") ||
		strings.Contains(q, "同时") ||
		strings.Contains(strings.ToLower(q), " and ") {
		s++
	}
	return s
}

func abstractionScore(q string) int {
	s := 0
	words := []string{
		"原理", "机制", "模型", "架构",
		"设计", "tradeoff", "design",
		"一致性", "复杂度", "可扩展",
	}
	lq := strings.ToLower(q)
	for _, w := range words {
		if strings.Contains(lq, strings.ToLower(w)) {
			s++
		}
	}
	return s
}

func nonTemplateScore(q string) int {
	s := 0
	if strings.Contains(q, "比较") || strings.Contains(q, "对比") {
		s += 2
	}
	if strings.Contains(q, "优缺点") || strings.Contains(q, "取舍") {
		s += 2
	}
	if strings.Contains(q, "设计一个") || strings.Contains(q, "方案") {
		s += 2
	}
	return s
}

func lengthScore(q string) int {
	l := len([]rune(q))
	if l >= 200 {
		return 2
	}
	if l >= 120 {
		return 1
	}
	return 0
}
