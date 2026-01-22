package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

/*
================================================
RUN MODE SWITCH（唯一需要改的地方）
------------------------------------------------
true  = 默认聊天即“长期记忆自我”（推荐）
false = 默认聊天仅即时回答（streamChat）
================================================
*/
const DefaultUseLongTermChat = true

// ==============================
// 可恢复输入错误（哨兵）
// ==============================
var ErrDirtyInput = errors.New("dirty input")

// ==============================
// Run（最终 UX 版）
// ==============================
func Run() {
	// ------------------------------
	// 0️⃣ 初始化
	// ------------------------------
	cfg := defaultConfig()
	mustEnsureDirs(cfg)
	mustEnsurePromptFiles(cfg)

	db := mustOpenDB(cfg)
	defer db.Close()

	lw := NewLogWriter(cfg, db)
	defer lw.Close()

	reader := bufio.NewReader(os.Stdin)

	fmt.Println("🧠 Local AI Chat")
	fmt.Println("Type exit to quit, /help for commands")
	fmt.Println()

	// ==============================
	// 1️⃣ 主循环
	// ==============================
	for {
		fmt.Print("You> ")

		line, err := readLine(reader)
		if err != nil {
			// ---------- 真正退出条件 ----------
			if errors.Is(err, io.EOF) {
				fmt.Println("\nbye")
				return
			}

			// ---------- 可恢复输入错误（中文输入法 / 编码） ----------
			if errors.Is(err, ErrDirtyInput) {
				fmt.Println("⚠️ 输入法异常，已忽略，请重新输入")
				continue
			}

			// ---------- 其他 stdin 错误 ----------
			fmt.Println("stdin error:", err)
			continue
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 统一退出（显式）
		if line == "exit" {
			return
		}

		// ------------------------------
		// 2️⃣ 命令模式（/xxx）
		// ------------------------------
		if strings.HasPrefix(line, "/") {
			handleCommand(cfg, db, lw, reader, line)
			fmt.Println("\n------------------\n")
			continue
		}

		// ------------------------------
		// 3️⃣ Markdown fence 多行
		// ------------------------------
		var input string
		if line == "```" {
			input, err = readUntilFence(reader)
			if err != nil {
				if errors.Is(err, ErrDirtyInput) {
					fmt.Println("⚠️ 输入法异常，已忽略")
					fmt.Println("\n------------------\n")
					continue
				}
				fmt.Println("input error:", err)
				fmt.Println("\n------------------\n")
				continue
			}
		} else {
			input = line
		}

		input = strings.TrimSpace(input)
		if input == "" {
			fmt.Println("\n------------------\n")
			continue
		}

		// ------------------------------
		// 4️⃣ 默认聊天入口
		// ------------------------------
		fmt.Println("\nAssistant>")

		if DefaultUseLongTermChat {
			if err := Chat(lw, cfg, db, input); err != nil {
				fmt.Println("chat error:", err)
			}
		} else {
			answer := streamChat(cfg, input)

			_ = lw.WriteRecord(map[string]string{
				"role":    "user",
				"content": input,
			})
			_ = lw.WriteRecord(map[string]string{
				"role":    "assistant",
				"content": answer,
			})
		}

		fmt.Println("\n------------------\n")
	}
}

// ==============================
// 输入校验（只拒绝，不退出）
// ==============================

// rejectDirtyInput：
// - 拒绝 Unicode Replacement Character（�）
// - 中文输入法回退时常见
func rejectDirtyInput(s string) error {
	if strings.ContainsRune(s, utf8.RuneError) {
		return ErrDirtyInput
	}
	return nil
}

// ==============================
// 输入工具函数
// ==============================

// readLine：读取单行（canonical stdin）
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		// EOF 直接向上抛，交给 Run 决定是否退出
		return "", err
	}

	line = strings.TrimRight(line, "\r\n")

	// 🚨 只拒绝本次输入，不终止程序
	if err := rejectDirtyInput(line); err != nil {
		return "", err
	}

	return line, nil
}

// readMultiline：空行提交（用于 /paste）
func readMultiline(r *bufio.Reader) (string, error) {
	var lines []string

	for {
		line, err := readLine(r)
		if err != nil {
			return "", err
		}

		if strings.TrimSpace(line) == "" {
			break
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}

// readUntilFence：``` 结束
func readUntilFence(r *bufio.Reader) (string, error) {
	var lines []string

	for {
		line, err := readLine(r)
		if err != nil {
			return "", err
		}

		if strings.TrimSpace(line) == "```" {
			break
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}
