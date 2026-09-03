package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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
		case "resolve":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "resolve requires a hostname")
				os.Exit(2)
			}
			addresses, err := net.LookupHost(os.Args[2])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			sort.Strings(addresses)
			fmt.Printf("addresses=%s\n", strings.Join(addresses, ","))
			return
		case "fetch":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "fetch requires a URL")
				os.Exit(2)
			}
			client := &http.Client{
				Timeout:   2 * time.Second,
				Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
			}
			response, err := client.Get(os.Args[2])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
			if err != nil || len(body) > 4096 {
				fmt.Fprintln(os.Stderr, "response body is unavailable or too large")
				os.Exit(1)
			}
			fmt.Printf("status=%d body=%s", response.StatusCode, body)
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
	var server *http.Server
	if port := strings.TrimSpace(os.Getenv("ORCH_E2E_HTTP_PORT")); port != "" {
		parsedPort, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsedPort == 0 {
			fmt.Fprintln(os.Stderr, "ORCH_E2E_HTTP_PORT must be a valid non-zero port")
			os.Exit(2)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) {
			if os.Getenv("ORCH_E2E_HEALTH_FAIL") == "1" {
				http.Error(response, "unhealthy", http.StatusServiceUnavailable)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("/identity", func(response http.ResponseWriter, request *http.Request) {
			local, _ := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
			address := ""
			if local != nil {
				address, _, _ = net.SplitHostPort(local.String())
			}
			_, _ = fmt.Fprintf(response, "address=%s revision=%s\n", address, os.Getenv("ORCH_E2E_REVISION"))
		})
		mux.HandleFunc("/", func(response http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(response, "token=%s\n", token)
		})
		server = &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: time.Second}
		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "HTTP server failed: %v\n", err)
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
			}
		}()
	}

	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM, syscall.SIGINT)
	<-terminated
	if server != nil {
		_ = server.Close()
	}
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
