package app

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

/*
================================================
Embedding Guard
- Summary embedding 漂移检测
- 超阈值报警，不覆盖主 embedding
================================================
*/

const (
	DriftWarnThreshold  = 0.15
	DriftBlockThreshold = 0.25
)

type EmbeddingWarning struct {
	Level   string // WARN / BLOCK
	Message string
}

// ========================
// Public Entry
// ========================

func CheckEmbeddingDrift(
	db *sql.DB,
	summaryID int64,
	newVec []float32,
) *EmbeddingWarning {

	oldVec, ok := loadLastEmbedding(db, summaryID)
	if !ok {
		return nil // 第一次，无对比
	}

	d := cosineDistance(oldVec, newVec)

	if d > DriftBlockThreshold {
		return &EmbeddingWarning{
			Level: "BLOCK",
			Message: fmt.Sprintf(
				"Embedding drift %.3f exceeds block threshold %.2f",
				d, DriftBlockThreshold,
			),
		}
	}

	if d > DriftWarnThreshold {
		return &EmbeddingWarning{
			Level: "WARN",
			Message: fmt.Sprintf(
				"Embedding drift %.3f exceeds warning threshold %.2f",
				d, DriftWarnThreshold,
			),
		}
	}

	return nil
}

// ========================
// History Storage
// ========================

func saveEmbeddingHistory(db *sql.DB, summaryID int64, vec []float32) {
	_, _ = db.Exec(`
		INSERT INTO summary_embeddings_history(summary_id, vec, created_at)
		VALUES(?,?,?)
	`, summaryID, encodeVec(vec), time.Now().Format(time.RFC3339))
}

func loadLastEmbedding(db *sql.DB, summaryID int64) ([]float32, bool) {
	row := db.QueryRow(`
		SELECT vec
		FROM summary_embeddings_history
		WHERE summary_id=?
		ORDER BY created_at DESC
		LIMIT 1
	`, summaryID)

	var blob []byte
	if err := row.Scan(&blob); err != nil {
		return nil, false
	}

	return decodeVec(blob), true
}

// ========================
// Vector Math
// ========================

func cosineDistance(a, b []float32) float64 {
	if len(a) != len(b) {
		return 1
	}

	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}

	if na == 0 || nb == 0 {
		return 1
	}

	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

func encodeVec(v []float32) []byte {
	buf := new(bytes.Buffer)
	for _, x := range v {
		_ = binary.Write(buf, binary.LittleEndian, x)
	}
	return buf.Bytes()
}

func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	buf := bytes.NewReader(b)
	for i := 0; i < n; i++ {
		_ = binary.Read(buf, binary.LittleEndian, &out[i])
	}
	return out
}
