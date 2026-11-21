package vm

import (
	"os"
	"strings"
)

const osReleaseFile = "/etc/os-release"

func OSRelease() (string, string) {
	data, err := os.ReadFile(osReleaseFile)
	if err != nil {
		return "", ""
	}
	var name, version string
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "NAME":
			name = value
		case "VERSION_ID":
			version = value
		}
	}
	return name, version
}
