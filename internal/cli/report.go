package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/refusal"
	"github.com/getzoorik/zoorikctl/internal/text"
	"github.com/getzoorik/zoorikctl/internal/ui"
)

const redactedToken = "<redacted>"

var connectTokenValue = regexp.MustCompile("zoorik_v1_[^\\s\"'`]+")

type redactingWriter struct {
	target io.Writer
}

func (w redactingWriter) Write(data []byte) (int, error) {
	if _, err := w.target.Write(connectTokenValue.ReplaceAll(data, []byte(redactedToken))); err != nil {
		return 0, err
	}
	return len(data), nil
}

type runLog struct {
	file *os.File
	out  io.Writer
}

func openRunLog(args []string) *runLog {
	if len(args) < 2 || args[0] != "cluster" || args[1] != "connect" {
		return nil
	}
	file, err := os.CreateTemp("", "zoorikctl-*.log")
	if err != nil {
		return nil
	}
	log := &runLog{file: file, out: redactingWriter{target: file}}
	fmt.Fprintf(log.out, "zoorikctl %s (%s/%s)\nstarted %s\ncommand: zoorikctl %s\n\n",
		api.Version(), runtime.GOOS, runtime.GOARCH, time.Now().UTC().Format(time.RFC3339),
		strings.Join(redact(args), " "))
	return log
}

func (l *runLog) writer() io.Writer {
	if l == nil {
		return nil
	}
	return l.out
}

func (l *runLog) discard() {
	if l == nil {
		return
	}
	name := l.file.Name()
	_ = l.file.Close()
	_ = os.Remove(name)
}

func (l *runLog) keep(err error, code int) string {
	if l == nil {
		return ""
	}
	fmt.Fprintf(l.out, "\nerror: %v\nexit code: %d\nfinished %s\n", err, code,
		time.Now().UTC().Format(time.RFC3339))
	_ = l.file.Close()
	return l.file.Name()
}

func redact(args []string) []string {
	redacted := make([]string, len(args))
	hideNext := false
	for index, arg := range args {
		name, value, hasValue := flagParts(arg)
		secret := strings.Contains(name, "token")
		switch {
		case hideNext:
			redacted[index] = redactedToken
			hideNext = false
		case secret && hasValue:
			redacted[index] = arg[:len(arg)-len(value)] + redactedToken
		case secret:
			redacted[index] = arg
			hideNext = true
		default:
			redacted[index] = arg
		}
	}
	return redacted
}

func flagParts(arg string) (string, string, bool) {
	trimmed := strings.TrimLeft(arg, "-—–")
	if trimmed == arg || trimmed == "" {
		return "", "", false
	}
	name, value, hasValue := strings.Cut(trimmed, "=")
	return strings.ToLower(name), value, hasValue
}

type declined struct {
	title       string
	interrupted bool
	next        string
}

func (d *declined) Error() string {
	return d.title
}

func (d *declined) print(p *ui.Printer) {
	if d.interrupted {
		p.Blank()
	}
	p.Plain(d.title)
	p.Dim(d.next)
}

func report(p *ui.Printer, err error) {
	var typed *refusal.Refusal
	switch {
	case errors.As(err, &typed):
		p.Fail(typed.What)
		for _, line := range typed.Detail {
			p.Dim(line)
		}
		if typed.Next != "" {
			p.Blank()
			p.Paragraph("  What to do: ", typed.Next)
		}
		if typed.Cause != nil {
			p.Blank()
			p.Dim("Reported by the cluster: " + text.OneLine(typed.Cause.Error()))
		}
	case errors.Is(err, context.Canceled):
		p.Fail("Canceled. Anything already applied is still there; re-running this command " +
			"finishes the install.")
	default:
		p.Fail("zoorikctl failed: " + err.Error())
	}
	p.Error("command failed")
}
