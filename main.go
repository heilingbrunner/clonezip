package main

import (
	"os"
	"runtime/debug"

	"github.com/heilingbrunner/clonezip/internal/cli"
)

var version = ""

func main() {
	cli.Version = resolveVersion()

	os.Exit(cli.Main(os.Args[1:]))
}

func resolveVersion() string {
	if version != "" {
		return version
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}

	return "dev"
}
