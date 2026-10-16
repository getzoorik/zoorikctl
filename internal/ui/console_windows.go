//go:build windows

package ui

import (
	"encoding/binary"
	"os"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func IsTerminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd())) || isMsysPty(file)
}

func prepareConsole(file *os.File) (bool, func()) {
	handle := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false, func() {}
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true, func() {}
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false, func() {}
	}
	return true, func() { _ = windows.SetConsoleMode(handle, mode) }
}

func hostGlyphs(terminal bool) (glyphLevel, bool) {
	if !terminal {
		return glyphASCII, false
	}
	emoji := os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") == "vscode"
	if emoji || strings.EqualFold(os.Getenv("ConEmuANSI"), "ON") || os.Getenv("TERM") != "" {
		return glyphUnicode, emoji
	}
	return glyphSafe, false
}

func isMsysPty(file *os.File) bool {
	handle := windows.Handle(file.Fd())
	if kind, err := windows.GetFileType(handle); err != nil || kind != windows.FILE_TYPE_PIPE {
		return false
	}
	buffer := make([]byte, 4+2*windows.MAX_PATH)
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileNameInfo, &buffer[0],
		uint32(len(buffer))); err != nil {
		return false
	}
	length := min(int(binary.LittleEndian.Uint32(buffer[:4]))/2, windows.MAX_PATH)
	units := make([]uint16, length)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(buffer[4+2*index:])
	}
	return isMsysPipeName(string(utf16.Decode(units)))
}

func isMsysPipeName(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) < 5 {
		return false
	}
	switch parts[0] {
	case `\msys`, `\cygwin`, `\Device\NamedPipe\msys`, `\Device\NamedPipe\cygwin`:
	default:
		return false
	}
	return parts[1] != "" && strings.HasPrefix(parts[2], "pty") &&
		(parts[3] == "from" || parts[3] == "to") && parts[4] == "master"
}
