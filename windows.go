//go:build windows

package main

import (
	"os/exec"
	"strings"
)

// openBrowser 尽量把默认浏览器拉起来。
//
// Windows 上“让系统自己打开一个网址”有几条等价的入口，其中任何一条都可能
// 因为默认浏览器关联损坏而静默失败（命令本身返回成功，浏览器却没起来），
// 所以这里依次尝试。真正的兜底在 main.go 的看门狗：若干秒内一个页面请求
// 都没收到，就把地址弹给用户。
func openBrowser(rawURL string) error {
	attempts := [][]string{
		{"cmd", "/c", "start", "", rawURL},
		{"rundll32", "url.dll,FileProtocolHandler", rawURL},
		{"explorer.exe", rawURL},
	}

	var lastErr error
	for _, attempt := range attempts {
		if lastErr = exec.Command(attempt[0], attempt[1:]...).Start(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// showNotice 弹一个系统提示框：本程序是无控制台的 windowsgui 程序，
// 弹窗是唯一能保证被用户看到的通道。
func showNotice(title, message string) {
	safeTitle := strings.ReplaceAll(title, "'", "''")
	safeMessage := strings.ReplaceAll(message, "'", "''")

	_ = exec.Command(
		"powershell",
		"-NoProfile",
		"-Command",
		"Add-Type -AssemblyName System.Windows.Forms; "+
			"[System.Windows.Forms.MessageBox]::Show('"+safeMessage+"','"+safeTitle+
			"',[System.Windows.Forms.MessageBoxButtons]::OK,"+
			"[System.Windows.Forms.MessageBoxIcon]::Information) | Out-Null",
	).Run()
}

func showFatalError(err error) {
	if err == nil {
		return
	}

	message := strings.ReplaceAll(err.Error(), "'", "''")
	_ = exec.Command(
		"powershell",
		"-NoProfile",
		"-Command",
		"Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('"+message+"','BiliLottery 启动失败')",
	).Run()
}
