package ui

import (
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
	"golang.org/x/text/width"
)

const (
	defaultWidth = 80
	minBoxWidth  = 60
	detailWidth  = 72
)

type glyphLevel int

const (
	glyphASCII glyphLevel = iota
	glyphSafe
	glyphUnicode
)

type capabilities struct {
	color   bool
	glyphs  glyphLevel
	emoji   bool
	animate bool
	width   int
	file    *os.File
}

func detect(w io.Writer) (capabilities, func()) {
	file, isFile := w.(*os.File)
	if !isFile {
		file = nil
	}
	terminal := file != nil && IsTerminal(file)

	virtual, restore := true, func() {}
	if file != nil && term.IsTerminal(int(file.Fd())) {
		virtual, restore = prepareConsole(file)
	}

	glyphs, emoji := hostGlyphs(terminal)
	caps := capabilities{
		color:   colorWanted(terminal && virtual),
		glyphs:  glyphs,
		emoji:   emoji,
		animate: terminal && virtual && os.Getenv("TERM") != "dumb" && !envSet("CI"),
		width:   defaultWidth,
	}
	if terminal {
		caps.file = file
		caps.width = caps.currentWidth()
	}
	return caps, restore
}

func (c capabilities) currentWidth() int {
	if c.file == nil {
		return c.width
	}
	if columns, _, err := term.GetSize(int(c.file.Fd())); err == nil && columns > 0 {
		return columns
	}
	return defaultWidth
}

func colorWanted(capable bool) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if value, ok := os.LookupEnv("FORCE_COLOR"); ok && strings.TrimSpace(value) != "" {
		return !falsy(value)
	}
	if value := os.Getenv("CLICOLOR_FORCE"); strings.TrimSpace(value) != "" && !falsy(value) {
		return true
	}
	if strings.TrimSpace(os.Getenv("CLICOLOR")) == "0" {
		return false
	}
	return capable && os.Getenv("TERM") != "dumb"
}

func falsy(value string) bool {
	value = strings.TrimSpace(value)
	return value == "0" || strings.EqualFold(value, "false")
}

func envSet(name string) bool {
	return strings.TrimSpace(os.Getenv(name)) != ""
}

func runeWidth(r rune) int {
	switch {
	case r == 0x200D || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1F000:
		return 2
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	total := 0
	escape := false
	for _, r := range s {
		switch {
		case escape:
			if r >= '@' && r <= '~' && r != '[' {
				escape = false
			}
		case r == '\x1b':
			escape = true
		default:
			total += runeWidth(r)
		}
	}
	return total
}

func truncateToWidth(s, ellipsis string, limit int) string {
	if limit <= 1 || displayWidth(s) <= limit {
		return s
	}
	room := limit - displayWidth(ellipsis)
	var builder strings.Builder
	used := 0
	for _, r := range s {
		step := runeWidth(r)
		if used+step > room {
			break
		}
		builder.WriteRune(r)
		used += step
	}
	return builder.String() + ellipsis
}
