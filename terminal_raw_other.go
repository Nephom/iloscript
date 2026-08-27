//go:build !linux

package main

import "errors"

// rawTermios 在非 Linux 平台上只是一個佔位型別，實際不會被使用。
type rawTermios struct{}

// enableCbreakMode 在非 Linux 平台上不支援即時單鍵偵測（例如 Ctrl+F 清空下方記錄），
// 因此直接回傳錯誤，呼叫端會據此優雅停用該功能，不影響其餘監控行為。
func enableCbreakMode(fd int) (*rawTermios, error) {
	return nil, errors.New("cbreak mode is not supported on this platform")
}

// restoreTerminal 在非 Linux 平台上為 no-op。
func restoreTerminal(fd int, orig *rawTermios) {}
