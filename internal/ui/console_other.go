//go:build !windows

package ui

import (
	"os"

	"golang.org/x/term"
)

func IsTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
}

func prepareConsole(*os.File) (bool, func()) {
	return true, func() {}
}

func hostGlyphs(bool) (glyphLevel, bool) {
	if os.Getenv("TERM") == "linux" {
		return glyphASCII, false
	}
	return glyphUnicode, true
}
