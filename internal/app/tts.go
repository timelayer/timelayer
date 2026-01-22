package app

import (
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"unicode"
)

//
// =====================================================
// Public API（业务层唯一入口）
// =====================================================
//

// Speak：异步、不阻塞
// - 只负责“投递朗读任务”
// - 不影响打印 / UI / 主流程
func Speak(text string) {
	onceInit.Do(initTTSWorker)

	text = prepareForSpeak(text)
	if text == "" {
		return
	}

	// 非阻塞投递：队列满了就丢弃（避免卡主流程）
	select {
	case ttsQueue <- text:
	default:
	}
}

//
// =====================================================
// TTS Worker（后台串行朗读）
// =====================================================
//

var (
	ttsQueue chan string
	onceInit sync.Once
)

// 启动唯一的 TTS worker
func initTTSWorker() {
	ttsQueue = make(chan string, 16) // 缓冲队列，防止阻塞调用方

	go func() {
		for text := range ttsQueue {
			speakBlocking(text)
		}
	}()
}

// 真正阻塞式朗读逻辑（只在 worker 中运行）
func speakBlocking(text string) {
	segs := splitMixedSegments(text)
	segs = mergeShortSegments(segs, 8) // 🔥 听感关键
	segs = splitLongSegments(segs, 240)
	segs = dropEmptySegments(segs)

	for _, seg := range segs {
		s := strings.TrimSpace(seg.Text)
		if s == "" {
			continue
		}

		switch runtime.GOOS {
		case "darwin":
			speakMac(s, seg.Lang)
		case "linux":
			speakLinux(s, seg.Lang)
		default:
			// unsupported OS: silently ignore
		}
	}
}

//
// =====================================================
// 文本预处理（只读“核心回答”）
// =====================================================
//

func prepareForSpeak(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// 去 refs / sources
	s = stripRefs(s)

	// 去 markdown
	s = stripMarkdown(s)

	// 防止朗读小说
	const maxRunes = 1200
	rs := []rune(s)
	if len(rs) > maxRunes {
		s = string(rs[:maxRunes]) + "…"
	}

	return strings.TrimSpace(s)
}

func stripRefs(s string) string {
	l := strings.ToLower(s)
	keys := []string{
		"\nrefs",
		"\nreferences",
		"\nsources",
	}
	for _, k := range keys {
		if i := strings.Index(l, k); i >= 0 {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

func stripMarkdown(s string) string {
	r := strings.NewReplacer(
		"`", "",
		"*", "",
		"_", "",
		"#", "",
		">", "",
		"- ", "",
	)
	return r.Replace(s)
}

//
// =====================================================
// OS 级 TTS（阻塞，由 worker 调用）
// =====================================================
//

func speakMac(text string, lang langType) {
	args := []string{"-r", "180"}

	switch lang {
	case langZH:
		args = append(args, "-v", "Tingting")
	case langEN:
		// 使用系统默认英文 voice
	default:
	}

	args = append(args, text)

	cmd := exec.Command("say", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

func speakLinux(text string, lang langType) {
	args := []string{}

	switch lang {
	case langZH:
		args = append(args, "-v", "zh")
	case langEN:
		args = append(args, "-v", "en")
	default:
	}

	args = append(args, text)

	cmd := exec.Command("espeak", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

//
// =====================================================
// 语言分段核心
// =====================================================
//

type langType int

const (
	langUnknown langType = iota
	langZH
	langEN
)

type segment struct {
	Lang langType
	Text string
}

func splitMixedSegments(s string) []segment {
	var segs []segment
	var buf []rune
	cur := langUnknown

	flush := func() {
		if len(buf) == 0 {
			return
		}
		segs = append(segs, segment{
			Lang: cur,
			Text: string(buf),
		})
		buf = buf[:0]
	}

	for _, r := range []rune(s) {
		t := classifyRune(r)

		// 标点 / 空白：缓存，不切段
		if t == langUnknown {
			buf = append(buf, r)
			continue
		}

		// 数字：跟随当前段，未知则 EN
		if isDigit(r) {
			if cur == langUnknown {
				cur = langEN
			}
			buf = append(buf, r)
			continue
		}

		if cur == langUnknown {
			cur = t
			buf = append(buf, r)
			continue
		}

		if t == cur {
			buf = append(buf, r)
			continue
		}

		flush()
		cur = t
		buf = append(buf, r)
	}

	flush()

	// 修正 unknown
	for i := range segs {
		if segs[i].Lang == langUnknown {
			if i > 0 {
				segs[i].Lang = segs[i-1].Lang
			} else {
				segs[i].Lang = langEN
			}
		}
	}

	return segs
}

func classifyRune(r rune) langType {
	switch {
	case isCJK(r):
		return langZH
	case isLatin(r):
		return langEN
	case isDigit(r):
		return langEN
	default:
		return langUnknown
	}
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x3000 && r <= 0x303F)
}

func isLatin(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

//
// =====================================================
// 分段后处理（极致听感）
// =====================================================
//

// - 纯标点永远不单独朗读
// - 标点 & 很短的段永远黏在前一句
func mergeShortSegments(segs []segment, minLen int) []segment {
	if len(segs) <= 1 {
		return segs
	}

	out := []segment{segs[0]}

	for i := 1; i < len(segs); i++ {
		cur := segs[i]
		t := strings.TrimSpace(cur.Text)

		// 🔥 纯标点
		if isAllPunctOrSpace(t) {
			out[len(out)-1].Text += cur.Text
			continue
		}

		// 🔥 极短段（OK / Yes / 好）
		if runeLen(t) < minLen {
			out[len(out)-1].Text += cur.Text
			continue
		}

		out = append(out, cur)
	}

	return out
}

// 丢弃不应朗读的段
func dropEmptySegments(segs []segment) []segment {
	out := segs[:0]
	for _, s := range segs {
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		if isAllPunctOrSpace(t) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// 太长的段拆开，避免 say / espeak 卡住
func splitLongSegments(segs []segment, maxLen int) []segment {
	var out []segment
	for _, seg := range segs {
		rs := []rune(seg.Text)
		if len(rs) <= maxLen {
			out = append(out, seg)
			continue
		}
		for i := 0; i < len(rs); i += maxLen {
			end := i + maxLen
			if end > len(rs) {
				end = len(rs)
			}
			out = append(out, segment{
				Lang: seg.Lang,
				Text: string(rs[i:end]),
			})
		}
	}
	return out
}

//
// =====================================================
// 工具函数
// =====================================================
//

func isAllPunctOrSpace(s string) bool {
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func runeLen(s string) int {
	return len([]rune(s))
}
