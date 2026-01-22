package app

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

/*
========================
Search Result Structure
========================
*/

type SearchHit struct {
	Score    float64 `json:"score"`     // rerank 后为最终分，否则等于 EmbScore
	EmbScore float64 `json:"emb_score"` // embedding cosine（仅 debug / 结构判断）
	Type     string  `json:"type"`
	Date     string  `json:"date"`
	Text     string  `json:"text"`
}

/*
========================
HTTP Client
========================
*/

var searchHTTPClient = &http.Client{
	Timeout: 120 * time.Second,
}

/*
========================
Rerank Intent Gate（只拦 reranker）
========================
*/

func shouldRerank(hits []SearchHit, cfg Config) bool {
	// 总开关
	if !cfg.EnableRerank {
		return false
	}

	// 强制 rerank（用于测试/对比/压测）：只要有 >=2 个候选就 rerank
	if cfg.RerankForce {
		return len(hits) >= 2
	}

	// 至少要有多个候选
	if len(hits) < cfg.RerankMinBatch {
		return false
	}

	// 只有一个候选，没必要 rerank
	if len(hits) == 1 {
		return false
	}

	top1 := hits[0].EmbScore
	top2 := hits[1].EmbScore
	gap := top1 - top2

	mode := strings.ToLower(strings.TrimSpace(cfg.RerankMode))
	if mode == "" {
		mode = "conservative"
	}

	switch mode {
	case "always":
		// 只要有 >=2 个候选就 rerank（仍然保留 EnableRerank / MinBatch）
		return true

	case "ambiguous":
		// 更符合 rerank 本质：embedding 分不清胜负时（gap 小）才用 cross-encoder 再判。
		// 仍然要求 query 语义足够强，否则容易把无关候选也拉去 rerank。
		if top1 < cfg.SearchMinStrong {
			return false
		}
		// 要求 top2 也不弱（否则只是 top1 一枝独秀，不是“难分胜负”）
		t2min := cfg.SearchMinStrong - cfg.SearchMinGap
		if t2min < cfg.SearchMinScore {
			t2min = cfg.SearchMinScore
		}
		if top2 < t2min {
			return false
		}
		return gap <= cfg.SearchMinGap*1.8

	case "smart":
		// “生产默认更稳”的折中：只要 top1 足够强，就 rerank。
		// 这会覆盖 ambiguous + obvious winner 两类情况，但不会在弱 query 上浪费开销。
		return top1 >= cfg.SearchMinStrong

	default: // conservative
		// 原逻辑：存在明显中心（gap 大）时 rerank；gap 小就跳过。
		if top1 < cfg.SearchMinStrong {
			return false
		}
		if gap < cfg.SearchMinGap*1.8 {
			return false
		}
		return true
	}
}

// ⭐ 仅用于 Debug：解释 rerank 被跳过的原因（不影响逻辑）
func explainRerankSkip(hits []SearchHit, cfg Config) string {
	if !cfg.EnableRerank {
		return "disabled"
	}
	if cfg.RerankForce {
		if len(hits) >= 2 {
			return "forced"
		}
		return "forced_but_insufficient_hits"
	}
	if len(hits) < cfg.RerankMinBatch {
		return "too_few_hits"
	}
	if len(hits) == 1 {
		return "single_hit"
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.RerankMode))
	if mode == "" {
		mode = "conservative"
	}

	top1 := hits[0].EmbScore
	top2 := hits[1].EmbScore
	gap := top1 - top2

	switch mode {
	case "always":
		return "mode_always"
	case "ambiguous":
		if top1 < cfg.SearchMinStrong {
			return "weak_query"
		}
		t2min := cfg.SearchMinStrong - cfg.SearchMinGap
		if t2min < cfg.SearchMinScore {
			t2min = cfg.SearchMinScore
		}
		if top2 < t2min {
			return "top2_too_weak"
		}
		if gap > cfg.SearchMinGap*1.8 {
			return "gap_too_large"
		}
		return "unknown"
	case "smart":
		if top1 < cfg.SearchMinStrong {
			return "weak_query"
		}
		return "unknown"
	default: // conservative
		if top1 < cfg.SearchMinStrong {
			return "weak_query"
		}
		if gap < cfg.SearchMinGap*1.8 {
			return "gap_too_small"
		}
		return "unknown"
	}
}

/*
========================
Public Search API
========================
*/

func SearchWithScore(db *sql.DB, cfg Config, query string) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// 1️⃣ embed query
	qv, qn, err := embedQueryText(cfg, query)
	if err != nil {
		return nil, err
	}
	if qn == 0 {
		return nil, nil
	}

	// 2️⃣ load embeddings
	rows, err := db.Query(`
		SELECT
			s.type,
			s.period_key,
			s.json,
			s.text,
			e.vec,
			e.l2,
			e.dim
		FROM embeddings e
		JOIN summaries s ON s.id = e.summary_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []SearchHit

	for rows.Next() {
		var (
			typ  string
			key  string
			js   string
			txt  string
			blob []byte
			l2   float64
			dim  int
		)

		if err := rows.Scan(&typ, &key, &js, &txt, &blob, &l2, &dim); err != nil {
			continue
		}
		if dim != len(qv) || l2 == 0 {
			continue
		}

		dot, ok := dotProductExactDim(qv, blob, dim)
		if !ok {
			continue
		}

		embScore := dot / (qn * l2)
		if math.IsNaN(embScore) || math.IsInf(embScore, 0) {
			continue
		}
		if embScore < cfg.SearchMinScore {
			continue
		}

		// 展示文本选择
		displayText := ""
		if typ == "fact" && strings.TrimSpace(txt) != "" {
			displayText = strings.TrimSpace(txt)
		} else {
			displayText = extractHumanText(js)
		}
		displayText = strings.TrimSpace(displayText)
		if displayText == "" {
			continue
		}

		hits = append(hits, SearchHit{
			Score:    embScore,
			EmbScore: embScore,
			Type:     typ,
			Date:     key,
			Text:     displayText,
		})
	}

	if len(hits) == 0 {
		return nil, nil
	}

	// 3️⃣ embedding 排序
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].EmbScore > hits[j].EmbScore
	})

	// 4️⃣ 截断给 rerank
	topN := cfg.RerankTopN
	if topN <= 0 {
		topN = cfg.SearchTopK
	}
	if topN < cfg.SearchTopK {
		topN = cfg.SearchTopK
	}
	if len(hits) > topN {
		hits = hits[:topN]
	}

	// 5️⃣ rerank（Intent Gate 在这里）
	if shouldRerank(hits, cfg) {
		docs := make([]string, 0, len(hits))
		for _, h := range hits {
			docs = append(docs, h.Text)
		}

		scores, rerr := rerankTexts(cfg, query, docs)
		if rerr == nil && len(scores) == len(hits) {
			for i := range hits {
				hits[i].Score = scores[i]
			}

			sort.SliceStable(hits, func(i, j int) bool {
				return hits[i].Score > hits[j].Score
			})

			printRerankDebug(hits)
		}
	} else {
		// ⭐ 新增：rerank 被跳过时的明确日志
		now := time.Now().Format("2006-01-02 15:04:05.000")
		reason := explainRerankSkip(hits, cfg)
		mode := strings.ToLower(strings.TrimSpace(cfg.RerankMode))

		// Add a tiny bit of numeric context to make tuning easier.
		if len(hits) >= 2 {
			top1 := hits[0].EmbScore
			top2 := hits[1].EmbScore
			gap := top1 - top2
			fmt.Printf(
				"========== RERANK SKIPPED @ %s mode=%s reason=%s hits=%d top1=%.4f top2=%.4f gap=%.4f strong=%.4f gap_th=%.4f ==========\n",
				now, mode, reason, len(hits), top1, top2, gap, cfg.SearchMinStrong, cfg.SearchMinGap,
			)
		} else {
			fmt.Printf(
				"========== RERANK SKIPPED @ %s mode=%s reason=%s hits=%d ==========\n",
				now, mode, reason, len(hits),
			)
		}
	}

	// 6️⃣ topK
	if len(hits) > cfg.SearchTopK {
		hits = hits[:cfg.SearchTopK]
	}

	return hits, nil
}

/*
========================
Query embedding
========================
*/

func embedQueryText(cfg Config, text string) ([]float32, float64, error) {
	payload := map[string]any{
		"input": text,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest("POST", cfg.EmbedURL, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf(
			"embed http error %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	vec, err := decodeEmbedding(raw)
	if err != nil {
		return nil, 0, err
	}

	return vec, l2norm(vec), nil
}

/*
========================
Human-readable summary extract
========================
*/

func extractHumanText(js string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		return strings.TrimSpace(js)
	}

	var lines []string
	if hs, ok := m["highlights"].([]any); ok {
		for _, h := range hs {
			lines = append(lines, fmt.Sprintf("- %v", h))
		}
	}
	if len(lines) == 0 {
		if t, ok := m["type"]; ok {
			lines = append(lines, fmt.Sprintf("summary type: %v", t))
		}
	}
	return strings.Join(lines, "\n")
}

/*
========================
Vector math
========================
*/

func dotProductExactDim(q []float32, blob []byte, dim int) (sum float64, ok bool) {
	if len(blob) < dim*4 {
		return 0, false
	}
	buf := bytes.NewReader(blob)
	for i := 0; i < dim; i++ {
		var x float32
		if err := binary.Read(buf, binary.LittleEndian, &x); err != nil {
			return 0, false
		}
		sum += float64(q[i] * x)
	}
	return sum, true
}

func l2norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x * x)
	}
	return math.Sqrt(s)
}

/*
========================
Debug
========================
*/

func printRerankDebug(hits []SearchHit) {
	n := len(hits)
	if n > 10 {
		n = 10
	}
	now := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Printf("========== RERANK DEBUG (top) @ %s ==========\n", now)
	for i := 0; i < n; i++ {
		h := hits[i]
		fmt.Printf(
			"[%02d] final=%.4f emb=%.4f type=%s date=%s text=%q\n",
			i, h.Score, h.EmbScore, h.Type, h.Date, cutForDebug(h.Text, 120),
		)
	}
	fmt.Println("==============================================")
}

func cutForDebug(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
