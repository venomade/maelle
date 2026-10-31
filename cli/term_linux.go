//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// rawState remembers the terminal's original termios so it can be restored.
type rawState struct {
	old syscall.Termios
}

// makeRaw puts the terminal into raw mode: no echo, no line buffering, no
// signal generation (Ctrl-C/Ctrl-D arrive as bytes), no output post-processing
// (the editor emits \r\n itself).
func makeRaw(f *os.File) (*rawState, error) {
	fd := f.Fd()
	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCGETS,
		uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IXON | syscall.ICRNL | syscall.BRKINT | syscall.INPCK | syscall.ISTRIP
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Oflag &^= syscall.OPOST
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TCSETS,
		uintptr(unsafe.Pointer(&raw)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	return &rawState{old: old}, nil
}

func (r *rawState) restore(f *os.File) {
	syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), syscall.TCSETS,
		uintptr(unsafe.Pointer(&r.old)), 0, 0, 0)
}

// detectWidth returns the terminal width, or 0 (no wrapping) when stdout is
// not a terminal — piped output must stay unwrapped so logs and tests can
// grep it.
func detectWidth() int {
	if !isCharDevice(os.Stdout) {
		return 0
	}
	var ws struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno == 0 && ws.Col > 0 {
		return int(ws.Col)
	}
	return 80
}

func watchResize(c chan<- os.Signal)     { signal.Notify(c, syscall.SIGWINCH) }
func stopWatchResize(c chan<- os.Signal) { signal.Stop(c) }
