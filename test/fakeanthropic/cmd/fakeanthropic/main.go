// Command fakeanthropic serves the fake Messages API of package
// test/fakeanthropic to tests outside Go: Spike S, the conductor's
// integration tests and the kind e2e suite.
//
//	fakeanthropic --listen 127.0.0.1:0 --scripts scripts.json \
//	  --record-dir /tmp/rec --binding-key-hex 00ff...
//
// It prints "fakeanthropic listening on <addr>" to stdout once it accepts
// connections, and logs one metadata line per request to stderr. SIGINT or
// SIGTERM shuts it down.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ChristopherJMiller/luma/test/fakeanthropic"
)

// defaultText answers a request without a script marker.
const defaultText = "fakeanthropic: this request carries no script marker."

// listenLine prefixes the address on the one line printed to stdout, which
// harnesses wait for and parse.
const listenLine = "fakeanthropic listening on "

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeanthropic:", err)
		os.Exit(1)
	}
}

// run parses args, serves until ctx is done, then shuts down.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fakeanthropic", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8090", "address to listen on (port 0 picks one)")
	recordDir := fs.String("record-dir", "", "also write every request to this directory")
	scriptsPath := fs.String("scripts", "", "JSON file mapping script names to steps (README \"Scripts\")")
	keyHex := fs.String("binding-key-hex", "", "thinking-signature HMAC key, hex")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	key, err := hex.DecodeString(*keyHex)
	if err != nil {
		return fmt.Errorf("--binding-key-hex: %w", err)
	}
	scripts, err := loadScripts(*scriptsPath)
	if err != nil {
		return err
	}
	fake := fakeanthropic.New(fakeanthropic.Options{
		RecordDir:   *recordDir,
		Scripts:     scripts,
		DefaultText: defaultText,
		BindingKey:  key,
	})

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s%s\n", listenLine, ln.Addr())

	srv := &http.Server{Handler: logRequests(fake, log.New(stderr, "", log.LstdFlags)), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadScripts reads a scripts file; an unknown field is an error, so a
// misspelt step key fails loudly instead of being ignored.
func loadScripts(path string) (map[string][]fakeanthropic.Step, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var scripts map[string][]fakeanthropic.Step
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scripts); err != nil {
		return nil, fmt.Errorf("--scripts %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b[dec.InputOffset():])) > 0 {
		return nil, fmt.Errorf("--scripts %s: trailing data after the scripts object", path)
	}
	return scripts, nil
}

// logRequests logs method, URI and status of each request: metadata only.
func logRequests(next http.Handler, l *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// %q escapes control characters, so a request cannot forge a log line.
		l.Printf("%q %q %d", r.Method, r.URL.RequestURI(), sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer's Flush.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
