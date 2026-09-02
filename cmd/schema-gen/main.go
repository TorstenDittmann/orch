package main

import (
	"flag"
	"fmt"
	"os"

	"change.codes/torsten/better-swarm/internal/config"
)

func main() {
	output := flag.String("out", "", "write schema to this path instead of stdout")
	flag.Parse()

	data, err := config.SchemaJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *output == "" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*output, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
