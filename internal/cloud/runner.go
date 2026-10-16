package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/kube"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const maxStderr = 2000

var notFoundMarkers = []string{
	"NoSuchEntity",
	"NonExistentQueue",
	"QueueDoesNotExist",
	"ResourceNotFoundException",
	"ResourceNotFound",
	"NotFoundException",
	"was not found",
	"could not be found",
	"does not exist",
}

type Command struct {
	Name string
	Args []string
}

func Cmd(name string, args ...string) Command {
	return Command{Name: name, Args: args}
}

func (c Command) String() string {
	words := make([]string, 0, len(c.Args)+1)
	words = append(words, kube.ShellWord(c.Name))
	for _, arg := range c.Args {
		words = append(words, kube.ShellWord(arg))
	}
	return strings.Join(words, " ")
}

type CommandError struct {
	Command Command
	Stderr  string
	Err     error
}

func (e *CommandError) Error() string {
	detail := strings.TrimSpace(e.Stderr)
	if detail == "" {
		detail = e.Err.Error()
	}
	return e.Command.Name + " " + firstWords(e.Command.Args, 3) + ": " + text.OneLine(detail)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

func NotFound(err error) bool {
	var failed *CommandError
	if !errors.As(err, &failed) {
		return false
	}
	for _, marker := range notFoundMarkers {
		if strings.Contains(failed.Stderr, marker) {
			return true
		}
	}
	return false
}

type Runner struct {
	OnRun func(Command)
}

func (r *Runner) Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (r *Runner) Output(ctx context.Context, command Command) ([]byte, error) {
	if r.OnRun != nil {
		r.OnRun(command)
	}
	process := exec.CommandContext(ctx, command.Name, command.Args...)
	process.Env = append(os.Environ(), "AWS_PAGER=", "AZURE_CORE_ONLY_SHOW_ERRORS=true",
		"AZURE_CORE_NO_COLOR=true")
	var stdout, stderr bytes.Buffer
	process.Stdout = &stdout
	process.Stderr = &stderr
	if err := process.Run(); err != nil {
		text := stderr.String()
		if len(text) > maxStderr {
			text = text[:maxStderr]
		}
		return stdout.Bytes(), &CommandError{Command: command, Stderr: text, Err: err}
	}
	return stdout.Bytes(), nil
}

func (r *Runner) JSON(ctx context.Context, command Command, into any) error {
	output, err := r.Output(ctx, command)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil
	}
	if err := json.Unmarshal(output, into); err != nil {
		return &CommandError{Command: command, Stderr: "the output is not JSON: " + text.OneLine(string(output)),
			Err: err}
	}
	return nil
}

func (r *Runner) Run(ctx context.Context, command Command) error {
	_, err := r.Output(ctx, command)
	return err
}

func (r *Runner) Helm(ctx context.Context, command Command, release helmRelease) error {
	if r.OnRun != nil {
		r.OnRun(command)
	}
	if err := release.apply(ctx); err != nil {
		return &CommandError{Command: command, Err: err}
	}
	return nil
}

func (r *Runner) HelmChart(command Command, chart, version string) error {
	if r.OnRun != nil {
		r.OnRun(command)
	}
	if err := readChart(chart, version); err != nil {
		return &CommandError{Command: command, Err: err}
	}
	return nil
}

func firstWords(args []string, count int) string {
	if len(args) < count {
		count = len(args)
	}
	return strings.Join(args[:count], " ")
}

func compactJSON(value any) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}
