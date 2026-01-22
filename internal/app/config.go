package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultChatURL   = "http://localhost:8080/v1/chat/completions"
	defaultEmbedURL  = "http://localhost:8080/embedding"
	defaultChatModel = "Qwen3-8B-Q5_K_M.gguf"

	defaultHTTPAddr = "127.0.0.1:3210"

	// ✅ Python rerank proxy（你现在跑在 8090）
	defaultRerankURL = "http://127.0.0.1:8090/v1/rerank_text"
)

type Config struct {
	BaseDir            string
	LogDir             string
	ArchiveDir         string
	PromptDir          string
	DBPath             string
	Location           *time.Location
	KeepRawDays        int
	MaxDailyJSONLBytes int64
	HTTPTimeout        time.Duration

	SearchTopK     int
	SearchMinScore float64

	// ⭐ Rerank Intent Gate（只影响 rerank，不影响 search）
	SearchMinStrong float64 // embedding 强度阈值（是否有明确语义中心）
	SearchMinGap    float64 // top1-top2 最小差距（是否值得 rerank）

	// ---- LLM / Embedding ----
	ChatURL   string
	EmbedURL  string
	ChatModel string

	// ---- Rerank ----
	EnableRerank   bool
	RerankForce    bool   // if true, force rerank whenever there are >=2 hits (testing/benchmarking)
	RerankMode     string // conservative | ambiguous | smart | always (see shouldRerank)
	RerankURL      string
	RerankTopN     int           // 先取 embedding topN，再用 rerank 重排
	RerankTimeout  time.Duration // 单次 rerank 请求超时
	RerankMinBatch int           // 少于这个数量不 rerank（节省开销）

	// ---- Web ----
	HTTPAddr                 string
	HTTPAuthToken            string // optional; if set, API requires X-Auth-Token or Authorization: Bearer
	HTTPAllowInsecureRemote  bool   // if true, allow binding to non-loopback without auth token
	HTTPRateLimitRPM         int    // simple per-IP rate limit for API endpoints
	HTTPMaxConcurrentStreams int    // limit concurrent /api/chat/stream
	HTTPMaxInputBytes        int    // max bytes for chat input

	// ---- SQLite ----
	SQLiteBusyTimeoutMS int
	SQLiteJournalMode   string // WAL recommended
	SQLiteSynchronous   string // NORMAL recommended
	SQLiteMaxOpenConns  int

	// ---- Recent Raw ----
	// 最近原始对话注入的最大行数（jsonl 的最后 N 行）。
	// 这个值越大，上下文承接能力越强，但 prompt 更长、污染风险也更高。
	RecentMaxLines int
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "local-ai")
	loc := time.Local // ✅ 使用系统时区

	cfg := Config{
		BaseDir:            base,
		LogDir:             filepath.Join(base, "logs"),
		ArchiveDir:         filepath.Join(base, "logs", "archive"),
		PromptDir:          filepath.Join(base, "prompts"),
		DBPath:             filepath.Join(base, "memory", "memory.sqlite"),
		Location:           loc,
		KeepRawDays:        45,
		MaxDailyJSONLBytes: 25 * 1024 * 1024, // 25MB
		HTTPTimeout:        600 * time.Second,

		SearchTopK:     5,
		SearchMinScore: 0.75,

		// ⭐ rerank intent gate 默认值（推荐）
		SearchMinStrong: 0.90,
		SearchMinGap:    0.05,

		ChatURL:   defaultChatURL,
		EmbedURL:  defaultEmbedURL,
		ChatModel: defaultChatModel,

		EnableRerank:   true,
		RerankForce:    false,
		RerankMode:     "smart", // conservative|ambiguous|smart|always
		RerankURL:      defaultRerankURL,
		RerankTopN:     20,               // ✅ 推荐：SearchTopK 的 4x 左右
		RerankTimeout:  15 * time.Second, // ✅ 你本地跑，一般够了
		RerankMinBatch: 2,

		HTTPAddr:                 defaultHTTPAddr,
		HTTPAuthToken:            "",
		HTTPAllowInsecureRemote:  false,
		HTTPRateLimitRPM:         120,
		HTTPMaxConcurrentStreams: 4,
		HTTPMaxInputBytes:        64 * 1024,

		SQLiteBusyTimeoutMS: 5000,
		SQLiteJournalMode:   "WAL",
		SQLiteSynchronous:   "NORMAL",
		SQLiteMaxOpenConns:  1,

		// recent raw
		RecentMaxLines: 20,
	}

	// ENV overrides (optional)
	if v := os.Getenv("TIMELAYER_CHAT_URL"); v != "" {
		cfg.ChatURL = v
	}
	if v := os.Getenv("TIMELAYER_EMBED_URL"); v != "" {
		cfg.EmbedURL = v
	}
	if v := os.Getenv("TIMELAYER_CHAT_MODEL"); v != "" {
		cfg.ChatModel = v
	}
	if v := os.Getenv("TIMELAYER_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("TIMELAYER_HTTP_AUTH_TOKEN"); v != "" {
		cfg.HTTPAuthToken = v
	}
	if v := os.Getenv("TIMELAYER_HTTP_ALLOW_INSECURE_REMOTE"); v != "" {
		if v == "1" || v == "true" || v == "TRUE" || v == "True" {
			cfg.HTTPAllowInsecureRemote = true
		}
	}
	if v := os.Getenv("TIMELAYER_HTTP_RATE_LIMIT_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.HTTPRateLimitRPM = n
		}
	}
	if v := os.Getenv("TIMELAYER_HTTP_MAX_CONCURRENT_STREAMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HTTPMaxConcurrentStreams = n
		}
	}
	if v := os.Getenv("TIMELAYER_HTTP_MAX_INPUT_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HTTPMaxInputBytes = n
		}
	}
	if v := os.Getenv("TIMELAYER_RECENT_MAX_LINES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RecentMaxLines = n
		}
	}

	// ---- Rerank ENV ----
	if v := os.Getenv("TIMELAYER_ENABLE_RERANK"); v != "" {
		// 允许：true/false/1/0
		if v == "1" || v == "true" || v == "TRUE" || v == "True" {
			cfg.EnableRerank = true
		} else {
			cfg.EnableRerank = false
		}
	}
	if v := os.Getenv("TIMELAYER_RERANK_FORCE"); v != "" {
		// 允许：true/false/1/0
		if v == "1" || v == "true" || v == "TRUE" || v == "True" {
			cfg.RerankForce = true
		} else {
			cfg.RerankForce = false
		}
	}
	if v := os.Getenv("TIMELAYER_RERANK_MODE"); v != "" {
		// conservative | ambiguous | smart | always
		m := strings.ToLower(strings.TrimSpace(v))
		switch m {
		case "conservative", "ambiguous", "smart", "always":
			cfg.RerankMode = m
		default:
			// keep default
		}
	}
	if v := os.Getenv("TIMELAYER_RERANK_URL"); v != "" {
		cfg.RerankURL = v
	}
	if v := os.Getenv("TIMELAYER_RERANK_TOPN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RerankTopN = n
		}
	}
	if v := os.Getenv("TIMELAYER_RERANK_TIMEOUT_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			cfg.RerankTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	if v := os.Getenv("TIMELAYER_RERANK_MIN_BATCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RerankMinBatch = n
		}
	}

	// ---- Search Intent Gate ENV (only affects rerank gating) ----
	if v := os.Getenv("TIMELAYER_SEARCH_MIN_STRONG"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			// clamp to sane range
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			cfg.SearchMinStrong = f
		}
	}
	if v := os.Getenv("TIMELAYER_SEARCH_MIN_GAP"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			cfg.SearchMinGap = f
		}
	}

	// ---- SQLite ENV ----
	if v := os.Getenv("TIMELAYER_SQLITE_BUSY_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.SQLiteBusyTimeoutMS = n
		}
	}
	if v := os.Getenv("TIMELAYER_SQLITE_JOURNAL_MODE"); v != "" {
		cfg.SQLiteJournalMode = v
	}
	if v := os.Getenv("TIMELAYER_SQLITE_SYNCHRONOUS"); v != "" {
		cfg.SQLiteSynchronous = v
	}
	if v := os.Getenv("TIMELAYER_SQLITE_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SQLiteMaxOpenConns = n
		}
	}

	return cfg
}

// DefaultConfig exposes the default config for other entrypoints (e.g., web server).
func DefaultConfig() Config {
	return defaultConfig()
}
