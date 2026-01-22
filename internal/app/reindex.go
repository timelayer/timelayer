package app

import (
	"database/sql"
	"fmt"
)

/*
========================
Reindex (Embedding Backfill)
llama-server / 1:1 embeddings
========================
*/

func Reindex(db *sql.DB, cfg Config, typ string) error {
	var (
		rows *sql.Rows
		err  error
	)

	switch typ {
	case "daily", "weekly", "monthly":
		rows, err = db.Query(`
			SELECT id, type, period_key, json
			FROM summaries
			WHERE type = ?
			ORDER BY period_key
		`, typ)

	case "all":
		rows, err = db.Query(`
			SELECT id, type, period_key, json
			FROM summaries
			ORDER BY type, period_key
		`)

	default:
		return fmt.Errorf("unknown reindex type: %s", typ)
	}

	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		total   int
		created int
		skipped int
		failed  int
	)

	for rows.Next() {
		var (
			id  int64
			sty string
			key string
			js  string
		)

		if err := rows.Scan(&id, &sty, &key, &js); err != nil {
			failed++
			continue
		}
		total++

		// ✅ 1:1 embedding：已有就跳过
		if hasEmbedding(db, id) {
			skipped++
			continue
		}

		// 从 JSON 中提取适合 embedding 的文本
		indexText := extractIndexText(js)
		if indexText == "" {
			skipped++
			continue
		}

		// 写入 embedding
		if err := ensureEmbedding(db, cfg, indexText, sty, key); err != nil {
			fmt.Printf(
				"[warn] embed failed %s %s: %v\n",
				sty, key, err,
			)
			failed++
			continue
		}

		fmt.Printf("[ok] embedded %s %s\n", sty, key)
		created++
	}

	fmt.Printf(
		"[reindex done] total=%d created=%d skipped=%d failed=%d\n",
		total, created, skipped, failed,
	)

	return nil
}
