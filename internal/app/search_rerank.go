package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type RerankTextRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type RerankTextResponse struct {
	Scores []float64 `json:"scores"`
	// 下面这些字段你的 proxy 也会返回，但 Go 端不一定要用
	RankedIndices   []int    `json:"ranked_indices,omitempty"`
	RankedDocuments []string `json:"ranked_documents,omitempty"`
}

func rerankTexts(cfg Config, query string, docs []string) ([]float64, error) {
	if !cfg.EnableRerank {
		return nil, nil
	}
	if strings.TrimSpace(cfg.RerankURL) == "" {
		return nil, fmt.Errorf("rerank enabled but RerankURL is empty")
	}
	if len(docs) < cfg.RerankMinBatch {
		return nil, nil
	}

	reqBody := RerankTextRequest{
		Query:     query,
		Documents: docs,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", cfg.RerankURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: cfg.RerankTimeout,
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		return nil, fmt.Errorf("rerank http %d: %s", resp.StatusCode, msg)
	}

	var out RerankTextResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	if len(out.Scores) != len(docs) {
		return nil, fmt.Errorf("rerank response length mismatch: scores=%d docs=%d", len(out.Scores), len(docs))
	}

	// 防止极端情况挂死：如果 proxy 或模型卡住，你可以更激进地降超时
	_ = time.Now()

	return out.Scores, nil
}
