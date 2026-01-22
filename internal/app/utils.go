package app

import (
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func mustEnsureDirs(cfg Config) {
	_ = os.MkdirAll(cfg.LogDir, 0755)
	_ = os.MkdirAll(cfg.ArchiveDir, 0755)
	_ = os.MkdirAll(cfg.PromptDir, 0755)
	_ = os.MkdirAll(filepath.Dir(cfg.DBPath), 0755)
}

// Week range: Monday..Sunday
func weekRange(d time.Time, loc *time.Location) (start, end time.Time) {
	dd := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	wd := int(dd.Weekday()) // Sunday=0
	offset := (wd + 6) % 7  // Monday->0 ... Sunday->6
	start = dd.AddDate(0, 0, -offset)
	end = start.AddDate(0, 0, 6)
	return
}

func monthRange(d time.Time, loc *time.Location) (start, end time.Time) {
	dd := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	start = time.Date(dd.Year(), dd.Month(), 1, 0, 0, 0, 0, loc)
	end = start.AddDate(0, 1, 0).AddDate(0, 0, -1)
	return
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sanitizeUTF8 确保字符串是合法 UTF-8。
// 如果包含非法字节，会通过 rune 重构，
// 将非法部分替换为 �，避免污染日志与后续 JSON / Prompt。
func sanitizeUTF8(s string) string {
	if s == "" {
		return s
	}

	// ✅ 最重要：只要不是合法 UTF-8，就按 rune 级别重建（丢弃非法字节）
	// 这样不会因为“恰好包含 RuneError”而误伤本来合法的字符（比如用户真的输入了“�”）。
	var b strings.Builder
	b.Grow(len(s))

	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			// 非法字节：丢弃
			s = s[1:]
			continue
		}
		s = s[size:]

		// 过滤控制字符（保留换行/制表）
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}

	clean := strings.TrimSpace(b.String())
	if clean == "" {
		return ""
	}

	// ✅ 压缩多余空格（仅压缩连续空格，不影响换行/制表）
	var out strings.Builder
	out.Grow(len(clean))
	prevSpace := false
	for _, r := range clean {
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
			out.WriteRune(r)
			continue
		}
		prevSpace = false
		out.WriteRune(r)
	}
	clean = out.String()

	// 最后兜底
	if !utf8.ValidString(clean) {
		return string([]rune(clean))
	}
	return clean
}
