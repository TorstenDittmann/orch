package main

import (
	"os"

	"change.codes/torsten/better-swarm/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
