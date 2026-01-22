package app

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"math"
	"sort"
	"strings"
	"time"
)

type PendingFactGroup struct {
	GroupID string        `json:"group_id"`
	Rep     PendingFact   `json:"rep"`
	Items   []PendingFact `json:"items"`
	Size    int           `json:"size"`
}

const pendingClusterThreshold = 0.88

func getPendingFactEmbedding(db *sql.DB, pendingFactID int64) (dim int, blob []byte, l2 float64, ok bool) {
	if db == nil || pendingFactID <= 0 {
		return 0, nil, 0, false
	}
	row := db.QueryRow(`SELECT dim, vec, l2 FROM pending_fact_embeddings WHERE pending_fact_id=? LIMIT 1`, pendingFactID)
	if err := row.Scan(&dim, &blob, &l2); err != nil {
		return 0, nil, 0, false
	}
	if dim <= 0 || len(blob) == 0 || l2 == 0 {
		return 0, nil, 0, false
	}
	return dim, blob, l2, true
}

func upsertPendingFactEmbedding(db *sql.DB, pendingFactID int64, vec []float32, l2 float64, createdAt string) error {
	if db == nil || pendingFactID <= 0 || len(vec) == 0 || l2 == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, v := range vec {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	_, _ = db.Exec(`DELETE FROM pending_fact_embeddings WHERE pending_fact_id=?`, pendingFactID)
	_, err := db.Exec(`
        INSERT INTO pending_fact_embeddings(pending_fact_id, dim, vec, l2, created_at)
        VALUES(?,?,?,?,?)
    `, pendingFactID, len(vec), buf.Bytes(), l2, createdAt)
	return err
}

func decodeVecBlob(blob []byte, dim int) []float32 {
	if dim <= 0 || len(blob) < dim*4 {
		return nil
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		bits := binary.LittleEndian.Uint32(blob[i*4 : i*4+4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

func cosine(a []float32, aL2 float64, b []float32, bL2 float64) float64 {
	if len(a) == 0 || len(a) != len(b) || aL2 == 0 || bL2 == 0 {
		return 0
	}
	var dot float64
	for i := 0; i < len(a); i++ {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (aL2 * bL2)
}

type pendingVec struct {
	v  []float32
	l2 float64
}

// ListPendingFactGroups returns pending facts grouped by semantic similarity.
// Best-effort: if embedding fails, that item becomes a singleton group.
func ListPendingFactGroups(cfg Config, db *sql.DB, limit int) ([]PendingFactGroup, error) {
	items, err := ListPendingFacts(db, limit)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	// sort by confidence desc, then created_at desc
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Confidence != items[j].Confidence {
			return items[i].Confidence > items[j].Confidence
		}
		return items[i].CreatedAt > items[j].CreatedAt
	})

	vecs := make(map[int64]pendingVec, len(items))
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc).Format(time.RFC3339)

	ensureVec := func(p PendingFact) pendingVec {
		if pv, ok := vecs[p.ID]; ok {
			return pv
		}
		dim, blob, l2, ok := getPendingFactEmbedding(db, p.ID)
		if ok {
			pv := pendingVec{v: decodeVecBlob(blob, dim), l2: l2}
			vecs[p.ID] = pv
			return pv
		}
		// compute embedding (best-effort)
		v, l2n, err := embedQueryText(cfg, p.Fact)
		if err == nil && len(v) > 0 && l2n > 0 {
			_ = upsertPendingFactEmbedding(db, p.ID, v, l2n, now)
			pv := pendingVec{v: v, l2: l2n}
			vecs[p.ID] = pv
			return pv
		}
		pv := pendingVec{v: nil, l2: 0}
		vecs[p.ID] = pv
		return pv
	}

	type grp struct {
		id    string
		rep   PendingFact
		repV  pendingVec
		items []PendingFact
	}
	var groups []grp
	gid := 0
	for _, it := range items {
		pv := ensureVec(it)

		// fast path: exact same fact_key -> force merge
		merged := false
		for gi := range groups {
			if groups[gi].rep.FactKey != "" && it.FactKey != "" && groups[gi].rep.FactKey == it.FactKey {
				groups[gi].items = append(groups[gi].items, it)
				merged = true
				break
			}
		}
		if merged {
			continue
		}

		// if no embedding, singleton
		if len(pv.v) == 0 {
			gid++
			groups = append(groups, grp{id: "g" + itoa64(int64(gid)), rep: it, repV: pv, items: []PendingFact{it}})
			continue
		}

		bestIdx := -1
		bestSim := 0.0
		for gi := range groups {
			if len(groups[gi].repV.v) == 0 {
				continue
			}
			sim := cosine(pv.v, pv.l2, groups[gi].repV.v, groups[gi].repV.l2)
			if sim > bestSim {
				bestSim = sim
				bestIdx = gi
			}
		}
		if bestIdx >= 0 && bestSim >= pendingClusterThreshold {
			groups[bestIdx].items = append(groups[bestIdx].items, it)
			// keep representative as the highest-confidence item (already sorted)
		} else {
			gid++
			groups = append(groups, grp{id: "g" + itoa64(int64(gid)), rep: it, repV: pv, items: []PendingFact{it}})
		}
	}

	// sort groups: size desc then rep confidence desc
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].items) != len(groups[j].items) {
			return len(groups[i].items) > len(groups[j].items)
		}
		return groups[i].rep.Confidence > groups[j].rep.Confidence
	})

	out := make([]PendingFactGroup, 0, len(groups))
	for _, g := range groups {
		// stable sort items by confidence desc
		sort.SliceStable(g.items, func(i, j int) bool {
			if g.items[i].Confidence != g.items[j].Confidence {
				return g.items[i].Confidence > g.items[j].Confidence
			}
			return strings.Compare(g.items[i].Fact, g.items[j].Fact) < 0
		})
		out = append(out, PendingFactGroup{
			GroupID: g.id,
			Rep:     g.rep,
			Items:   g.items,
			Size:    len(g.items),
		})
	}
	return out, nil
}
