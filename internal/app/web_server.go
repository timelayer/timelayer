package app

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"io/fs"
)

//go:embed web/*
var webFS embed.FS

type apiChatReq struct {
	// input is used by /api/chat and /api/chat/stream
	Input string `json:"input"`
	// question is used by the web UI debug overlay (/api/debug/context)
	// kept for backward/forward compatibility with older web assets.
	Question string `json:"question"`
}

type apiChatResp struct {
	Text string `json:"text"`
}

type apiPendingFactsResp struct {
	Count int           `json:"count"`
	Items []PendingFact `json:"items"`
}

type apiPendingFactsCountResp struct {
	Count int `json:"count"`
}

type apiFactCountsResp struct {
	Pending   int `json:"pending"`
	Conflicts int `json:"conflicts"`
}

type apiBatchActionReq struct {
	IDs []int64 `json:"ids"`
}

type apiConflictActionReq struct {
	ID          int64  `json:"id"`
	Replacement string `json:"replacement"`
}

type apiPendingActionReq struct {
	ID int64 `json:"id"`
}

const maxJSONBodyBytes = 1 << 20 // 1MB

// ============================================================
// StartWeb
// ============================================================

func StartWeb(cfg Config, db *sql.DB, lw *LogWriter) error {
	if db == nil {
		return nil
	}

	// Safe-by-default: refuse non-loopback bind unless an auth token is set, or user explicitly allows insecure remote bind.
	if !cfg.HTTPAllowInsecureRemote && cfg.HTTPAuthToken == "" && !isLoopbackListenAddr(cfg.HTTPAddr) {
		return fmt.Errorf("refusing to bind to %s without auth; set TIMELAYER_HTTP_AUTH_TOKEN or TIMELAYER_HTTP_ALLOW_INSECURE_REMOTE=1", cfg.HTTPAddr)
	}

	if cfg.Location == nil {
		cfg.Location = time.Local
	}

	streamSem := make(chan struct{}, maxInt(1, cfg.HTTPMaxConcurrentStreams))

	mux := http.NewServeMux()

	// =========================
	// Web UI
	// =========================
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.Handle("/static/",
		http.StripPrefix(
			"/static/",
			http.FileServer(http.FS(mustSubFS(webFS, "web"))),
		),
	)

	// =========================
	// Health
	// =========================
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// =========================
	// Pending facts API
	// =========================
	mux.HandleFunc("/api/facts/status/counts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(apiFactCountsResp{Pending: CountPendingFacts(db), Conflicts: CountFactConflicts(db)})
	})

	// Alias for README/diagram friendliness
	mux.HandleFunc("/api/facts/counts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(apiFactCountsResp{Pending: CountPendingFacts(db), Conflicts: CountFactConflicts(db)})
	})

	mux.HandleFunc("/api/facts/pending/count", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(apiPendingFactsCountResp{Count: CountPendingFacts(db)})
	})

	mux.HandleFunc("/api/facts/pending", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		items, err := ListPendingFacts(db, 60)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(apiPendingFactsResp{Count: len(items), Items: items})
	})

	// REST-ish aliases to match README/diagram style:
	//   POST /api/facts/pending/:id/remember
	//   POST /api/facts/pending/:id/reject
	mux.HandleFunc("/api/facts/pending/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/facts/pending/")
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		action := parts[1]
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch action {
		case "remember":
			out, err := RememberPendingFact(cfg, db, id)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": out})
		case "reject":
			if err := RejectPendingFact(cfg, db, id); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
			return
		}
	})

	mux.HandleFunc("/api/facts/pending/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		groups, err := ListPendingFactGroups(cfg, db, 60)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "groups": groups})
	})

	mux.HandleFunc("/api/facts/remember", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiPendingActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if req.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		out, err := RememberPendingFact(cfg, db, req.ID)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": out})
	})

	mux.HandleFunc("/api/facts/remember_batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiBatchActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		out, err := RememberPendingFactsBatch(cfg, db, req.IDs)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcomes": out})
	})

	mux.HandleFunc("/api/facts/reject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiPendingActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if req.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := RejectPendingFact(cfg, db, req.ID); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/facts/reject_batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiBatchActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if err := RejectPendingFactsBatch(cfg, db, req.IDs); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	//
	// =========================
	// Active facts + history
	// =========================
	mux.HandleFunc("/api/facts/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		items, err := ListActiveFacts(db, 200)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "items": items})
	})

	mux.HandleFunc("/api/facts/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit := parseIntClamp(r.URL.Query().Get("limit"), 200, 1, 500)
		items, err := ListUserFactHistory(db, limit)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "items": items})
	})

	// =========================
	// Fact conflicts API
	// =========================
	mux.HandleFunc("/api/facts/conflicts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		items, err := ListFactConflicts(db, 60)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "items": items, "count": len(items)})
	})

	mux.HandleFunc("/api/facts/conflicts/keep", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiPendingActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if req.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := ResolveFactConflictKeep(db, req.ID, time.Now().In(cfg.Location)); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/facts/conflicts/replace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiConflictActionReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		if req.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := ResolveFactConflictReplace(cfg, db, req.ID, req.Replacement, time.Now().In(cfg.Location)); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	// Convenience REST-style endpoint (alias)
	//   POST /api/facts/conflicts/:id/resolve
	// Body:
	//   {"action":"keep"}
	//   {"action":"replace","replacement":"..."}
	mux.HandleFunc("/api/facts/conflicts/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/facts/conflicts/")
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) != 2 || parts[1] != "resolve" {
			http.NotFound(w, r)
			return
		}
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || id <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			Action      string `json:"action"`
			Replacement string `json:"replacement"`
		}
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		now := time.Now().In(cfg.Location)
		switch strings.ToLower(strings.TrimSpace(req.Action)) {
		case "keep":
			if err := ResolveFactConflictKeep(db, id, now); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		case "replace":
			if err := ResolveFactConflictReplace(cfg, db, id, req.Replacement, now); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("invalid action: expected keep|replace"))
			return
		}
	})

	// =========================
	// Debug: context injection audit
	// =========================
	auditHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req apiChatReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		// Accept both {"input": "..."} and legacy {"question": "..."}
		q := strings.TrimSpace(req.Input)
		if q == "" {
			q = strings.TrimSpace(req.Question)
		}
		if q == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if cfg.HTTPMaxInputBytes > 0 && len(q) > cfg.HTTPMaxInputBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		date := time.Now().In(cfg.Location).Format("2006-01-02")
		audit := BuildChatContextAudit(cfg, db, date, q)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// Web UI expects the audit object at top-level.
		_ = json.NewEncoder(w).Encode(audit)
	}
	mux.HandleFunc("/api/debug/context", auditHandler)
	// Alias for README/diagram friendliness
	mux.HandleFunc("/api/context/audit", auditHandler)

	// =========================
	// Non-stream chat
	// =========================
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req apiChatReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		req.Input = strings.TrimSpace(req.Input)
		if req.Input == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if cfg.HTTPMaxInputBytes > 0 && len(req.Input) > cfg.HTTPMaxInputBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		// ===== 1️⃣ 命令优先（CLI 同源）=====
		if strings.HasPrefix(req.Input, "/") {
			handled, out, err := HandleCommandWeb(cfg, db, lw, req.Input)
			if handled {
				if err != nil {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(err.Error()))
					return
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_ = json.NewEncoder(w).Encode(apiChatResp{Text: out})
				return
			}
		}

		// ===== 2️⃣ 普通对话（LLM）=====
		ans, err := ChatOnceWithContext(r.Context(), lw, cfg, db, req.Input, false, nil)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(apiChatResp{Text: ans})
	})

	// =========================
	// Stream chat (SSE)
	// =========================
	mux.HandleFunc("/api/chat/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		fl, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		var req apiChatReq
		if err := decodeJSONLimited(w, r, &req, maxJSONBodyBytes); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		req.Input = strings.TrimSpace(req.Input)
		if req.Input == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if cfg.HTTPMaxInputBytes > 0 && len(req.Input) > cfg.HTTPMaxInputBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// ping
		_, _ = w.Write([]byte(":ok\n\n"))
		fl.Flush()

		// ===== 1️⃣ 命令模式（一次性返回）=====
		if strings.HasPrefix(req.Input, "/") {
			cmd, _ := normalizeCommand(req.Input)
			handled, out, err := HandleCommandWeb(cfg, db, lw, req.Input)
			if handled {
				if err != nil {
					writeSSE(w, fl, map[string]string{"error": err.Error()})
					return
				}

				// Facts ops are designed to be "silent" in chat: only refresh LEDs/counters.
				out = strings.TrimSpace(out)
				silentFacts := (cmd == "/remember" || cmd == "/forget" || cmd == "/pending_add") &&
					(strings.HasPrefix(out, "[ok]") || strings.HasPrefix(out, "[noop]"))
				if cmd == "/remember" || cmd == "/forget" || cmd == "/pending_add" {
					_ = writeSSE(w, fl, map[string]string{"notice": "facts"})
					if !silentFacts && out != "" {
						_ = writeSSE(w, fl, map[string]string{"delta": out})
					}
					_ = writeSSE(w, fl, map[string]string{"done": "1"})
					return
				}

				_ = writeSSE(w, fl, map[string]string{"delta": out})
				_ = writeSSE(w, fl, map[string]string{"done": "1"})
				return
			}
		}

		// limit concurrent streams
		select {
		case streamSem <- struct{}{}:
			defer func() { <-streamSem }()
		default:
			_ = writeSSE(w, fl, map[string]string{"error": "too many concurrent streams"})
			_ = writeSSE(w, fl, map[string]string{"done": "1"})
			return
		}

		// ===== 2️⃣ 普通对话（流式 LLM）=====
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		_, err := ChatOnceWithContext(ctx, lw, cfg, db, req.Input, false, func(delta string) {
			select {
			case <-ctx.Done():
				return
			default:
			}

			if err := writeSSE(w, fl, map[string]string{"delta": delta}); err != nil {
				cancel() // 触发上游取消
				return
			}
		})

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			_ = writeSSE(w, fl, map[string]string{"error": err.Error()})
			return
		}

		_ = writeSSE(w, fl, map[string]string{"done": "1"})
		time.Sleep(10 * time.Millisecond)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           applyHTTPMiddleware(cfg, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	return srv.ListenAndServe()
}

// ============================================================
// helpers
// ============================================================

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func parseIntClamp(s string, def int, minV int, maxV int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if n < minV {
		return minV
	}
	if maxV > 0 && n > maxV {
		return maxV
	}
	return n
}

func decodeJSONLimited(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	// reject trailing tokens (except whitespace)
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid json")
	}
	return nil
}

func writeSSE(w http.ResponseWriter, fl http.Flusher, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return err
	}
	fl.Flush()
	return nil
}
