package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type llmResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
}

// Non-stream call (used by ask / summaries)
func callLLMNonStream(cfg Config, prompt string) (string, error) {
	payload := map[string]any{
		"model": cfg.ChatModel,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"stream": false,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", cfg.ChatURL, bytes.NewReader(b))
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

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm http error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var r llmResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("no choices; body=%s", strings.TrimSpace(string(body)))
	}

	if c := strings.TrimSpace(r.Choices[0].Message.Content); c != "" {
		return c, nil
	}
	if t := strings.TrimSpace(r.Choices[0].Text); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("empty content in choices")
}
