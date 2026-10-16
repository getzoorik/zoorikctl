package ui

import (
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
)

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	faint  = "\x1b[38;5;245m"
	green  = "\x1b[38;5;34m"
	red    = "\x1b[38;5;196m"
	orange = "\x1b[38;5;214m"
	blue   = "\x1b[38;5;39m"
)

const (
	bannerInner    = minBoxWidth + 1
	narrowestFrame = minBoxWidth / 2
)

type glyphs struct {
	ok, fail, warn, info, bullet, ellipsis string
	h, v, tl, tr, bl, br                   string
	spinner                                []string
}

var unicodeGlyphs = glyphs{
	ok: "✓", fail: "✕", warn: "!", info: "ℹ", bullet: "•", ellipsis: "…",
	h: "─", v: "│", tl: "╭", tr: "╮", bl: "╰", br: "╯",
	spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
}

var safeGlyphs = glyphs{
	ok: "√", fail: "×", warn: "!", info: "i", bullet: "·", ellipsis: "...",
	h: "─", v: "│", tl: "┌", tr: "┐", bl: "└", br: "┘",
	spinner: []string{"|", "/", "-", "\\"},
}

var asciiGlyphs = glyphs{
	ok: "+", fail: "x", warn: "!", info: "i", bullet: "*", ellipsis: "...",
	h: "-", v: "|", tl: "+", tr: "+", bl: "+", br: "+",
	spinner: []string{"|", "/", "-", "\\"},
}

type Printer struct {
	out     io.Writer
	log     io.Writer
	caps    capabilities
	glyph   glyphs
	restore func()
	active  *Spinner
	mu      sync.Mutex
}

func New(out io.Writer) *Printer {
	caps, restore := detect(out)
	glyph := asciiGlyphs
	switch caps.glyphs {
	case glyphUnicode:
		glyph = unicodeGlyphs
	case glyphSafe:
		glyph = safeGlyphs
	}
	return &Printer{out: out, caps: caps, glyph: glyph, restore: restore}
}

func (p *Printer) Tee(log io.Writer) *Printer {
	p.log = log
	return p
}

func (p *Printer) Halt() {
	if p.active != nil {
		p.active.halt()
	}
}

func (p *Printer) Close() {
	p.Halt()
	if p.restore != nil {
		p.restore()
		p.restore = nil
	}
}

func (p *Printer) width() int {
	return p.caps.currentWidth()
}

func (p *Printer) paint(code, text string) string {
	if !p.caps.color || text == "" {
		return text
	}
	return code + text + reset
}

func (p *Printer) emit(styled, plain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.out, styled)
	if p.log != nil {
		fmt.Fprintln(p.log, plain)
	}
}

func (p *Printer) raw(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprint(p.out, text)
}

func (p *Printer) Blank() {
	p.emit("", "")
}

func (p *Printer) Banner(title string) {
	if p.caps.emoji {
		title = "🚀 " + title
	}
	inner := max(bannerInner, displayWidth(title)+6)
	if inner+2 > p.width()-1 {
		p.emit(p.paint(bold, title), title)
		return
	}
	padding := strings.Repeat(" ", inner-3-displayWidth(title))
	top := p.glyph.tl + strings.Repeat(p.glyph.h, inner) + p.glyph.tr
	bottom := p.glyph.bl + strings.Repeat(p.glyph.h, inner) + p.glyph.br
	p.emit(p.paint(blue, top), top)
	p.emit(p.paint(blue, p.glyph.v)+"   "+title+padding+p.paint(blue, p.glyph.v),
		p.glyph.v+"   "+title+padding+p.glyph.v)
	p.emit(p.paint(blue, bottom), bottom)
}

func (p *Printer) Section(title string) {
	p.Blank()
	p.emit(p.paint(bold, title), title)
}

func (p *Printer) Plain(text string) {
	p.emit(text, text)
}

func (p *Printer) Success(text string) {
	p.mark(green, p.glyph.ok, text)
}

func (p *Printer) Fail(text string) {
	p.mark(red, p.glyph.fail, text)
}

func (p *Printer) Warn(text string) {
	p.mark(orange, p.glyph.warn, text)
}

func (p *Printer) Info(text string) {
	p.mark(blue, p.glyph.info, text)
}

func (p *Printer) mark(code, glyph, text string) {
	p.emit("  "+p.paint(code, glyph)+" "+text, "  "+glyph+" "+text)
}

func (p *Printer) Field(key, value string) {
	p.emit("  "+p.paint(blue, key)+": "+value, "  "+key+": "+value)
}

func (p *Printer) Item(text string) {
	p.emit("    "+p.paint(faint, p.glyph.bullet)+" "+text, "    "+p.glyph.bullet+" "+text)
}

func (p *Printer) Detail(text string) {
	for _, line := range wrap(text, min(p.width()-4, detailWidth)) {
		p.emit("  "+p.paint(faint, line), "  "+line)
	}
}

func (p *Printer) Error(text string) {
	p.emit("Error: "+text, "Error: "+text)
}

func (p *Printer) Prompt(text string) {
	p.raw("  " + p.paint(blue, "?") + " " + text + " ")
	p.Log("  ? " + text)
}

func (p *Printer) Raw(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.out, text)
}

func (p *Printer) Dim(text string) {
	lines := []string{text}
	if displayWidth(text) > p.width()-5 {
		lines = wrap(text, p.width()-5)
	}
	for _, line := range lines {
		p.emit("    "+p.paint(faint, line), "    "+line)
	}
}

func (p *Printer) Paragraph(lead, text string) {
	hanging := strings.Repeat(" ", displayWidth(lead))
	for index, line := range wrap(text, p.width()-1-displayWidth(lead)) {
		prefix := hanging
		if index == 0 {
			prefix = lead
		}
		p.emit(prefix+line, prefix+line)
	}
}

func (p *Printer) Log(text string) {
	if p.log == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.log, text)
}

type BoxLine struct {
	Text   string
	Indent int
	Bullet bool
}

type boxRow struct {
	plain, styled string
}

func (p *Printer) SuccessBox(title string, lines []BoxLine) {
	p.box(green, p.glyph.ok, title, lines)
}

func (p *Printer) WarnBox(title string, lines []BoxLine) {
	p.box(orange, p.glyph.warn, title, lines)
}

func (p *Printer) box(code, glyph, title string, lines []BoxLine) {
	heading := glyph + " " + title
	natural := displayWidth(heading)
	for _, line := range lines {
		rows, _ := p.boxRows(line, math.MaxInt)
		natural = max(natural, displayWidth(rows[0].plain))
	}

	limit := p.width() - 3
	inner := min(max(natural+2, minBoxWidth-2), limit)
	framed := limit >= narrowestFrame && displayWidth(heading) <= inner-2
	var body []boxRow
	for _, line := range lines {
		rows, fits := p.boxRows(line, inner-2)
		framed = framed && fits
		body = append(body, rows...)
	}

	p.Blank()
	if !framed {
		p.emit(p.paint(code, heading), heading)
		for _, line := range lines {
			rows, _ := p.boxRows(line, math.MaxInt)
			p.emit(rows[0].styled, rows[0].plain)
		}
		return
	}

	top := p.glyph.tl + strings.Repeat(p.glyph.h, inner) + p.glyph.tr
	bottom := p.glyph.bl + strings.Repeat(p.glyph.h, inner) + p.glyph.br
	p.emit(p.paint(code, top), top)
	p.boxRow(code, inner, boxRow{plain: heading, styled: p.paint(code, heading)})
	p.boxRow(code, inner, boxRow{})
	for _, row := range body {
		p.boxRow(code, inner, row)
	}
	p.emit(p.paint(code, bottom), bottom)
}

func (p *Printer) boxRows(line BoxLine, limit int) ([]boxRow, bool) {
	words := strings.Fields(line.Text)
	if len(words) == 0 {
		return []boxRow{{}}, true
	}
	lead := strings.Repeat(" ", line.Indent)
	styledLead := lead
	if line.Bullet {
		styledLead = lead + p.paint(faint, p.glyph.bullet) + " "
		lead += p.glyph.bullet + " "
	}
	hanging := strings.Repeat(" ", displayWidth(lead))

	var rows []boxRow
	text := words[0]
	for _, word := range words[1:] {
		if displayWidth(lead)+displayWidth(text)+1+displayWidth(word) <= limit {
			text += " " + word
			continue
		}
		rows = append(rows, boxRow{plain: lead + text, styled: styledLead + text})
		lead, styledLead, text = hanging, hanging, word
	}
	rows = append(rows, boxRow{plain: lead + text, styled: styledLead + text})

	for _, row := range rows {
		if displayWidth(row.plain) > limit {
			return rows, false
		}
	}
	return rows, true
}

func (p *Printer) boxRow(code string, inner int, row boxRow) {
	padding := strings.Repeat(" ", max(0, inner-1-displayWidth(row.plain)))
	edge := p.paint(code, p.glyph.v)
	p.emit(edge+" "+row.styled+padding+edge, p.glyph.v+" "+row.plain+padding+p.glyph.v)
}

func wrap(text string, limit int) []string {
	if limit < 20 {
		limit = 20
	}
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		lead := paragraph[:len(paragraph)-len(strings.TrimLeft(paragraph, " "))]
		hanging := strings.Repeat(" ", displayWidth(lead)+bulletWidth(words))
		current := lead + words[0]
		for _, word := range words[1:] {
			if displayWidth(current)+1+displayWidth(word) > limit {
				lines = append(lines, current)
				current = hanging + word
				continue
			}
			current += " " + word
		}
		lines = append(lines, current)
	}
	return lines
}

func bulletWidth(words []string) int {
	if len(words) < 2 {
		return 0
	}
	switch words[0] {
	case "·", "•", "*", "-":
		return displayWidth(words[0]) + 1
	}
	return 0
}
