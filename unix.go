//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// openBrowser 在类 Unix 系统上拉起默认浏览器：
// macOS 用 open，Linux 优先 xdg-open，再退到 gio / sensible-browser。
// 都失败时交给 main.go 的看门狗提示用户手动打开。
func openBrowser(rawURL string) error {
	var attempts [][]string
	if runtime.GOOS == "darwin" {
		attempts = [][]string{
			{"open", rawURL},
		}
	} else {
		attempts = [][]string{
			{"xdg-open", rawURL},
			{"gio", "open", rawURL},
			{"sensible-browser", rawURL},
			{"x-www-browser", rawURL},
		}
	}

	var lastErr error
	for _, attempt := range attempts {
		if lastErr = exec.Command(attempt[0], attempt[1:]...).Start(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// showNotice 弹一个系统提示框（无控制台场景下的可见提示）：
// macOS 用 osascript；Linux 依次尝试 zenity、kdialog；都没有则静默（日志里仍有地址）。
func showNotice(title, message string) {
	if runtime.GOOS == "darwin" {
		script := "display dialog " + osaQuote(message) +
			" with title " + osaQuote(title) +
			" buttons {\"OK\"} default button 1"
		_ = exec.Command("osascript", "-e", script).Run()
		return
	}

	if err := exec.Command("zenity", "--info", "--title="+title, "--text="+message).Run(); err == nil {
		return
	}
	_ = exec.Command("kdialog", "--title", title, "--msgbox", message).Run()
}

// showFatalError 启动失败时的最后提示：写标准错误 + 尽力弹窗。
func showFatalError(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "BiliLottery 启动失败：", err)
	showNotice("BiliLottery 启动失败", err.Error())
}

// osaQuote 把文本包成 AppleScript 字符串字面量（转义反斜杠与双引号）。
func osaQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}
