package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			fmt.Printf("exec stdout token=%s\n", os.Getenv("ORCH_E2E_TOKEN"))
			fmt.Fprintf(os.Stderr, "exec stderr token=%s\n", os.Getenv("ORCH_E2E_TOKEN"))
			return
		case "network":
			interfaces, err := networkInterfaces("/proc/net/dev")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Printf("interfaces=%s\n", strings.Join(interfaces, ","))
			return
		case "resources":
			for _, name := range []string{"cpu.max", "memory.max", "memory.swap.max"} {
				contents, err := os.ReadFile("/sys/fs/cgroup/" + name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "read %s: %v\n", name, err)
					os.Exit(1)
				}
				fmt.Printf("%s=%s\n", name, strings.TrimSpace(string(contents)))
			}
			return
		case "memory":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "memory requires a MiB amount")
				os.Exit(2)
			}
			amount, err := strconv.Atoi(os.Args[2])
			if err != nil || amount < 1 || amount > 1024 {
				fmt.Fprintln(os.Stderr, "memory amount must be from 1 through 1024 MiB")
				os.Exit(2)
			}
			blocks := make([][]byte, 0, amount)
			for index := 0; index < amount; index++ {
				block := make([]byte, 1<<20)
				for offset := 0; offset < len(block); offset += 4096 {
					block[offset] = byte(index)
				}
				blocks = append(blocks, block)
			}
			fmt.Printf("allocated=%dMiB\n", len(blocks))
			runtime.KeepAlive(blocks)
			return
		}
	}

	token := os.Getenv("ORCH_E2E_TOKEN")
	if path := os.Getenv("ORCH_E2E_START_FILE"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open start file: %v\n", err)
			os.Exit(1)
		}
		if _, err := fmt.Fprintf(file, "%s\n", token); err != nil {
			_ = file.Close()
			fmt.Fprintf(os.Stderr, "write start file: %v\n", err)
			os.Exit(1)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			fmt.Fprintf(os.Stderr, "sync start file: %v\n", err)
			os.Exit(1)
		}
		if err := file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close start file: %v\n", err)
			os.Exit(1)
		}
	}

	args := strings.Join(os.Args[1:], ",")
	fmt.Printf("fixture stdout token=%s args=%s\n", token, args)
	fmt.Fprintf(os.Stderr, "fixture stderr token=%s args=%s\n", token, args)

	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM, syscall.SIGINT)
	<-terminated
}

func networkInterfaces(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open network device list: %w", err)
	}
	defer file.Close()
	var result []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		separator := strings.IndexByte(line, ':')
		if separator < 0 {
			continue
		}
		name := strings.TrimSpace(line[:separator])
		if name != "" {
			result = append(result, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read network device list: %w", err)
	}
	sort.Strings(result)
	return result, nil
}
