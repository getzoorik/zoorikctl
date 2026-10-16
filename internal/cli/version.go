package cli

import (
	"fmt"
	"runtime"

	"github.com/getzoorik/zoorikctl/internal/api"
)

func runVersion(streams Streams) error {
	fmt.Fprintf(streams.Out, "zoorikctl %s (%s/%s, built with %s)\n",
		api.Version(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}
