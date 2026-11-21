package vm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/getzoorik/zoorikctl/internal/api"
	"github.com/getzoorik/zoorikctl/internal/text"
)

const (
	EnvFile = "/etc/amoeba-agent.env"

	AgentUnit = "amoeba-agent"

	keptScriptLines = 10
)

func CLIUser() string {
	if os.Geteuid() == 0 {
		return os.Getenv("SUDO_USER")
	}
	return ""
}

func CheckSudo(in io.Reader, out io.Writer) error {
	if os.Geteuid() == 0 {
		return nil
	}
	check := exec.Command("sudo", "-v")
	check.Stdin, check.Stdout, check.Stderr = in, out, out
	return check.Run()
}

func WriteEnvFile(registered *api.VMRegistration) error {
	write := elevated("install", "-m", "600", "-o", "root", "-g", "root", "/dev/stdin", EnvFile)
	write.Stdin = strings.NewReader("AMOEBA_AGENT_AUTH_KEY=" + registered.AgentKey + "\n" +
		"AMOEBA_AGENT_SERVER_URL=" + registered.ServerURL + "\n" +
		"AMOEBA_AGENT_VM_RESOURCE_ID=" + registered.VMResourceID + "\n" +
		"AMOEBA_AGENT_CLOUD=" + registered.Cloud + "\n")
	if output, err := write.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, text.OneLine(string(output)))
	}
	return nil
}

func RunInstallScript(script string, line func(string)) (int, []string, error) {
	dir, err := os.MkdirTemp("", "zoorikctl-")
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		return 0, nil, err
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return 0, nil, err
	}
	install := elevated("/bin/bash", file)
	install.Stdout, install.Stderr = writer, writer
	err = install.Start()
	_ = writer.Close()
	if err != nil {
		_ = reader.Close()
		return 0, nil, err
	}

	var last []string
	buffered := bufio.NewReader(reader)
	for {
		output, readErr := buffered.ReadString('\n')
		if output = strings.TrimRight(output, "\r\n"); output != "" {
			line(output)
			last = append(last, output)
			if len(last) > keptScriptLines {
				last = last[1:]
			}
		}
		if readErr != nil {
			break
		}
	}
	_ = reader.Close()

	err = install.Wait()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return exited.ExitCode(), last, nil
	}
	return 0, last, err
}

func elevated(name string, args ...string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}
