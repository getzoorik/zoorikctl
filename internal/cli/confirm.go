package cli

import (
	"bufio"
	"context"
	"io"
	"strings"
)

func readAnswer(ctx context.Context, in io.Reader) (string, error) {
	if in == nil {
		return "", io.EOF
	}
	type line struct {
		text string
		err  error
	}
	answered := make(chan line, 1)
	go func() {
		text, err := bufio.NewReader(in).ReadString('\n')
		if err == io.EOF && text != "" {
			err = nil
		}
		answered <- line{text: strings.TrimSpace(text), err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case got := <-answered:
		return got.text, got.err
	}
}
