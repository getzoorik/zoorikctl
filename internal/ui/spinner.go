package ui

import (
	"sync"
	"time"
)

const (
	spinnerInterval = 90 * time.Millisecond
	wrapOff         = "\x1b[?7l"
	wrapOn          = "\x1b[?7h"
	clearLine       = "\r\x1b[2K"
)

type Spinner struct {
	printer  *Printer
	mu       sync.Mutex
	label    string
	stop     chan struct{}
	done     chan struct{}
	animated bool
}

func (p *Printer) Spin(label string) *Spinner {
	p.Halt()
	p.Log("  " + label)
	spinner := &Spinner{printer: p, label: label, animated: p.caps.animate}
	if spinner.animated {
		spinner.stop = make(chan struct{})
		spinner.done = make(chan struct{})
		p.active = spinner
		p.raw(wrapOff)
		go spinner.run()
	}
	return spinner
}

func (p *Printer) Pending(label string) *Spinner {
	p.Halt()
	p.Info(label)
	return &Spinner{printer: p, label: label}
}

func (s *Spinner) run() {
	defer close(s.done)
	ticker := time.NewTicker(spinnerInterval)
	defer ticker.Stop()
	p := s.printer
	frames := p.glyph.spinner
	for frame := 0; ; frame++ {
		s.mu.Lock()
		label := s.label
		s.mu.Unlock()
		line := truncateToWidth(label, p.glyph.ellipsis, p.width()-5)
		p.raw(clearLine + "  " + p.paint(blue, frames[frame%len(frames)]) + " " + line)
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Spinner) Note(text string) {
	s.printer.Log("    " + text)
}

func (s *Spinner) Tick(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
}

func (s *Spinner) halt() {
	if !s.animated || s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
	s.printer.raw(clearLine + wrapOn)
	if s.printer.active == s {
		s.printer.active = nil
	}
}

func (s *Spinner) Success(text string) {
	s.halt()
	s.printer.Success(text)
}

func (s *Spinner) Warn(text string) {
	s.halt()
	s.printer.Warn(text)
}

func (s *Spinner) Fail(text string) {
	s.halt()
	s.printer.Fail(text)
}

func (s *Spinner) Stop() {
	s.halt()
}
