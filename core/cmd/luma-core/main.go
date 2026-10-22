// Command luma-core is Core. `serve` runs the server (Task 15 wires it);
// `maintenance on|off|status` switches maintenance mode through the loopback
// admin API, run inside the pod with `kubectl exec` (S12).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/maintenance"
)

const usage = `usage: luma-core <command>

commands:
  serve                 run Core
  maintenance on [--reason TEXT] [--drain-timeout=120s]
                        refuse new turns, drain the running ones, close every CLI
  maintenance off       accept turns again; CLIs reopen lazily
  maintenance status    print enabled=<bool> running_turns=<n> open_hosts=<n>
`

const defaultAdminListen = "127.0.0.1:8082"

// errUsage marks a command line that is wrong, not a failed request.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run executes one command and returns the exit code: 0 ok, 1 failed, 2 a
// usage error (usage on stderr).
func run(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		fmt.Fprintln(stderr, "serve: not wired yet (Task 15)")
		return 1
	case "maintenance":
		err := runMaintenance(ctx, args[1:], env, stdout)
		switch {
		case errors.Is(err, errUsage):
			fmt.Fprintf(stderr, "%sluma-core: %v\n", usage, err)
			return 2
		case err != nil:
			fmt.Fprintf(stderr, "maintenance: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// adminURL is the admin API on LUMA_ADMIN_LISTEN (default 127.0.0.1:8082).
func adminURL(env func(string) string) string {
	addr := env("LUMA_ADMIN_LISTEN")
	if addr == "" {
		addr = defaultAdminListen
	}
	return "http://" + addr + maintenance.AdminPath
}

func runMaintenance(ctx context.Context, args []string, env func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: maintenance needs on, off or status", errUsage)
	}
	addr := env("LUMA_ADMIN_LISTEN")
	if addr == "" {
		addr = defaultAdminListen
	}
	if err := maintenance.CheckAdminAddr(addr); err != nil {
		return fmt.Errorf("%w: LUMA_ADMIN_LISTEN: %w", errUsage, err)
	}

	fs := flag.NewFlagSet("maintenance "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "why maintenance is on")
	drain := fs.Duration("drain-timeout", maintenance.DefaultDrainTimeout, "how long to wait for running turns before interrupting them")
	var body map[string]any
	timeout := 30 * time.Second
	switch args[0] {
	case "on":
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		if *drain < 0 {
			return fmt.Errorf("%w: --drain-timeout must not be negative", errUsage)
		}
		body = map[string]any{"enabled": true, "reason": *reason, "drain_timeout": drain.String()}
		timeout += *drain + time.Minute // the drain, its interrupt grace and the shutdown
	case "off", "status":
		if err := parse(flag.NewFlagSet("maintenance "+args[0], flag.ContinueOnError), args[1:]); err != nil {
			return err
		}
		if args[0] == "off" {
			body = map[string]any{"enabled": false}
		}
	default:
		return fmt.Errorf("%w: unknown maintenance command %q", errUsage, args[0])
	}

	method, payload := http.MethodGet, io.Reader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		method, payload = http.MethodPost, bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, adminURL(env), payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var s maintenance.Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return fmt.Errorf("unreadable status: %w", err)
	}
	_, err = fmt.Fprintln(stdout, s.Line())
	return err
}

// parse parses flags and allows no positional arguments.
func parse(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %s: %w", errUsage, fs.Name(), err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: %s takes no arguments, got %q", errUsage, fs.Name(), fs.Args())
	}
	return nil
}
