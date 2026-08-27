//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// 對應 Linux kernel 的 struct termios（用於 TCGETS / TCSETS ioctl），
// 欄位配置需與 <asm-generic/termbits.h> 完全一致，否則會讀寫到錯誤的記憶體位置。
type rawTermios struct {
	Iflag uint32
	Oflag uint32
	Cflag uint32
	Lflag uint32
	Line  uint8
	Cc    [19]uint8
}

const (
	tcgets = 0x5401
	tcsets = 0x5402

	lflagISIG   = 0x0001
	lflagICANON = 0x0002
	lflagECHO   = 0x0008

	ccVMIN  = 6
	ccVTIME = 5
)

func getTermios(fd int) (*rawTermios, error) {
	var t rawTermios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tcgets), uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func setTermios(fd int, t *rawTermios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tcsets), uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

// enableCbreakMode 關閉終端機的行緩衝 (ICANON) 與畫面回顯 (ECHO)，讓程式能夠
// 即時讀到單一按鍵（例如 Ctrl+F），不需要等使用者按下 Enter；
// 但特意保留 ISIG，讓 Ctrl+C／Ctrl+Z 依然由終端機驅動直接轉成 SIGINT／SIGTSTP，
// 不影響既有以 signal.Notify 實作的 Ctrl+C 停止監控機制。
// 回傳的 *rawTermios 是原始設定，供結束時還原終端機用。
func enableCbreakMode(fd int) (*rawTermios, error) {
	orig, err := getTermios(fd)
	if err != nil {
		return nil, err
	}

	raw := *orig
	raw.Lflag &^= lflagICANON | lflagECHO
	raw.Lflag |= lflagISIG
	raw.Cc[ccVMIN] = 1
	raw.Cc[ccVTIME] = 0

	if err := setTermios(fd, &raw); err != nil {
		return nil, err
	}
	return orig, nil
}

// restoreTerminal 把終端機還原成呼叫 enableCbreakMode 之前的設定。
func restoreTerminal(fd int, orig *rawTermios) {
	if orig == nil {
		return
	}
	_ = setTermios(fd, orig)
}
