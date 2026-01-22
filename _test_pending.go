package main

import (
  "database/sql"
  "encoding/json"
  "fmt"
  "strings"
  "time"

  _ "modernc.org/sqlite"
)

func extractFactSubject(fact string) string {
  fact = strings.TrimSpace(fact)
  if i := strings.Index(fact, "就是"); i > 0 { return strings.TrimSpace(fact[:i]) }
  if i := strings.Index(fact, "是"); i > 0 { return strings.TrimSpace(fact[:i]) }
  return ""
}

func normalizeFactKey(s string) string {
  s = strings.TrimSpace(s)
  s = strings.Join(strings.Fields(s), " ")
  s = strings.ToLower(s)
  return s
}

func deriveFactKeyFromSubject(content string) string {
  subject := extractFactSubject(content)
  if subject == "" { return normalizeFactKey(content) }
  return "subject:" + normalizeFactKey(subject)
}

func hasActiveUserFact(db *sql.DB, factKey string) bool {
  row := db.QueryRow(`SELECT 1 FROM user_facts WHERE is_active=1 AND fact_key=? LIMIT 1`, factKey)
  var one int
  return row.Scan(&one) == nil
}

func EnsurePendingFactsFromDailyJSON(db *sql.DB, date string, dailyJSON string) error {
  var obj map[string]any
  if err := json.Unmarshal([]byte(dailyJSON), &obj); err != nil { return err }
  explicitRaw, _ := obj["user_facts_explicit"]
  if explicitRaw == nil { return nil }
  v := explicitRaw.([]any)
  now := time.Now().Format(time.RFC3339)
  for _, it := range v {
    fact := strings.TrimSpace(it.(string))
    fk := deriveFactKeyFromSubject(fact)
    if hasActiveUserFact(db, fk) { continue }
    _, err := db.Exec(`INSERT INTO pending_facts(fact,fact_key,confidence,source_type,source_key,status,created_at,updated_at)
      VALUES(?,?,?,?,?,'pending',?,?)
      ON CONFLICT(fact_key, status, source_type, source_key) DO UPDATE SET
        fact=excluded.fact,
        confidence=max(pending_facts.confidence, excluded.confidence),
        updated_at=excluded.updated_at
    `, fact, fk, 0.85, "daily", date, now, now)
    if err != nil { return err }
  }
  return nil
}

func main() {
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil { panic(err) }
  _, _ = db.Exec(`CREATE TABLE user_facts (id INTEGER PRIMARY KEY AUTOINCREMENT, fact TEXT NOT NULL, fact_key TEXT NOT NULL, is_active INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(fact_key));`)
  _, _ = db.Exec(`CREATE TABLE pending_facts (id INTEGER PRIMARY KEY AUTOINCREMENT, fact TEXT NOT NULL, fact_key TEXT NOT NULL, confidence REAL NOT NULL DEFAULT 0.0, source_type TEXT NOT NULL, source_key TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(fact_key, status, source_type, source_key));`)

  js := `{"user_facts_explicit": ["记住：我最喜欢的颜色是黄色。", "我最喜欢的颜色是黄色。"]}`
  if err := EnsurePendingFactsFromDailyJSON(db, "2026-01-10", js); err != nil { panic(err) }
  rows, _ := db.Query(`SELECT fact,fact_key FROM pending_facts`)
  defer rows.Close()
  for rows.Next() {
    var f, k string
    _ = rows.Scan(&f, &k)
    fmt.Println(f, "=>", k)
  }
}

