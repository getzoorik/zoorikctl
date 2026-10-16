package api

import (
	"fmt"
	"runtime"
)

var version = "dev"

func Version() string { return version }

func UserAgent() string {
	return fmt.Sprintf("zoorikctl/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}
