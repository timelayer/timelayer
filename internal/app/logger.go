package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LogWriter struct {
	cfg        Config
	db         *sql.DB
	file       *os.File
	currentDay string

	mu             sync.Mutex
	lastRotatedDay string
}

func NewLogWriter(cfg Config, db *sql.DB) *LogWriter {
	return &LogWriter{
		cfg: cfg,
		db:  db,
	}
}

func (lw *LogWriter) Close() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.file != nil {
		_ = lw.file.Close()
		lw.file = nil
	}
}

func (lw *LogWriter) WriteRecord(rec map[string]string) error {
	now := time.Now().In(lw.cfg.Location)
	today := now.Format("2006-01-02")

	// ---------- ✅ UTF-8 清洗（关键修复点） ----------
	clean := make(map[string]string, len(rec))
	for k, v := range rec {
		clean[k] = sanitizeUTF8(v)
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return err
	}

	// ---------- 跨天处理（只触发一次 rollup） ----------
	var rotateDay string
	var doRotate bool

	lw.mu.Lock()
	if lw.currentDay != "" && lw.currentDay != today {
		yesterday := lw.currentDay
		if lw.file != nil {
			_ = lw.file.Close()
			lw.file = nil
		}
		lw.currentDay = ""

		if lw.lastRotatedDay != yesterday {
			doRotate = true
			rotateDay = yesterday
			lw.lastRotatedDay = yesterday
		}
	}

	// ---------- 打开当天日志 ----------
	if lw.file == nil {
		_ = os.MkdirAll(lw.cfg.LogDir, 0755)
		f, err := os.OpenFile(
			filepath.Join(lw.cfg.LogDir, today+".jsonl"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY,
			0644,
		)
		if err != nil {
			lw.mu.Unlock()
			return err
		}
		lw.file = f
		lw.currentDay = today
	}
	lw.mu.Unlock()

	// ---------- rollup / archive（不要持锁，避免阻塞写日志） ----------
	if doRotate && rotateDay != "" {
		lw.rollupAndArchive(rotateDay, today)
	}

	// ---------- 写入 ----------
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.file == nil {
		return fmt.Errorf("log file not open")
	}
	_, err = lw.file.Write(append(b, '\n'))
	return err
}

func (lw *LogWriter) rollupAndArchive(yesterday, today string) {
	// ---------- DAILY ----------
	if err := ensureDaily(lw.cfg, lw.db, yesterday, false); err != nil {
		fmt.Println("[warn] ensureDaily failed:", err)
	}

	// ---------- WEEKLY ----------
	yDate, _ := time.ParseInLocation("2006-01-02", yesterday, lw.cfg.Location)
	tDate, _ := time.ParseInLocation("2006-01-02", today, lw.cfg.Location)

	yYear, yWeek := yDate.ISOWeek()
	tYear, tWeek := tDate.ISOWeek()

	if yYear != tYear || yWeek != tWeek {
		weekKey := fmt.Sprintf("%04d-W%02d", yYear, yWeek)
		if err := ensureWeekly(lw.cfg, lw.db, weekKey, false); err != nil {
			fmt.Println("[warn] ensureWeekly failed:", err)
		}
	}

	// ---------- MONTHLY ----------
	yMonth := yDate.Format("2006-01")
	tMonth := tDate.Format("2006-01")

	if yMonth != tMonth {
		if err := ensureMonthly(lw.cfg, lw.db, yMonth, false); err != nil {
			fmt.Println("[warn] ensureMonthly failed:", err)
		}
	}

	// ---------- ARCHIVE ----------
	if err := forgetAndArchive(lw.cfg, lw.db); err != nil {
		fmt.Println("[warn] archive failed:", err)
	}
}
