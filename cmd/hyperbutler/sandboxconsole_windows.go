//go:build windows

package main

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
)

// cpUTF8 is the UTF-8 code page.
const cpUTF8 = 65001

// console puts this process's console in the mode a remote terminal needs: keys go to the
// session as VT sequences with no local line editing or echo, and its output is
// interpreted as VT. restore puts everything back. With stdin or stdout redirected (a
// script, another agent) it changes nothing and the terminal has the default size.
type console struct {
	in, out                    windows.Handle
	inMode, outMode            uint32
	inCP, outCP                uint32
	haveIn, haveOut            bool
	fallbackCols, fallbackRows uint16
}

func newConsole() (*console, error) {
	c := &console{in: windows.Handle(os.Stdin.Fd()), out: windows.Handle(os.Stdout.Fd()), fallbackCols: 80, fallbackRows: 24}
	if windows.GetConsoleMode(c.in, &c.inMode) != nil || windows.GetConsoleMode(c.out, &c.outMode) != nil {
		return c, nil
	}
	c.haveIn, c.haveOut = true, true
	c.inCP, _ = windows.GetConsoleCP()
	c.outCP, _ = windows.GetConsoleOutputCP()
	if err := windows.SetConsoleCP(cpUTF8); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleOutputCP(cpUTF8); err != nil {
		c.restore()
		return nil, err
	}
	in := c.inMode&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_QUICK_EDIT_MODE) |
		windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_EXTENDED_FLAGS
	if err := windows.SetConsoleMode(c.in, in); err != nil {
		c.restore()
		return nil, err
	}
	out := c.outMode | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if err := windows.SetConsoleMode(c.out, out); err != nil {
		c.restore()
		return nil, err
	}
	return c, nil
}

func (c *console) restore() {
	if c.haveIn {
		_ = windows.SetConsoleMode(c.in, c.inMode)
		_ = windows.SetConsoleCP(c.inCP)
	}
	if c.haveOut {
		_ = windows.SetConsoleMode(c.out, c.outMode)
		_ = windows.SetConsoleOutputCP(c.outCP)
	}
}

func (c *console) size() (cols, rows uint16) {
	var i windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &i); err != nil {
		return c.fallbackCols, c.fallbackRows
	}
	w, h := int(i.Window.Right)-int(i.Window.Left)+1, int(i.Window.Bottom)-int(i.Window.Top)+1
	if w <= 0 || h <= 0 {
		return c.fallbackCols, c.fallbackRows
	}
	return uint16(w), uint16(h) //nolint:gosec // G115: from int16 fields
}

// shownGuestText escapes what the guest said, on a line of its own.
func shownGuestText(b []byte) string {
	return review.Shorten(strings.TrimSpace(string(b)), 1024)
}
