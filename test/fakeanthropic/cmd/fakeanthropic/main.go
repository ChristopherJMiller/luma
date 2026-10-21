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

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to listen on (port 0 picks one)")
	recordDir := flag.String("record-dir", "", "also write every request to this directory")
	scripts := flag.String("scripts", "", "JSON file mapping script names to steps (README \"Scripts\")")
	keyHex := flag.String("binding-key-hex", "", "thinking-signature HMAC key, hex")
	flag.Parse()
	if err := run(*listen, *recordDir, *scripts, *keyHex); err != nil {
		fmt.Fprintln(os.Stderr, "fakeanthropic:", err)
		os.Exit(1)
	}
}

func run(listen, recordDir, scriptsPath, keyHex string) error {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return fmt.Errorf("--binding-key-hex: %w", err)
	}
	scripts, err := loadScripts(scriptsPath)
	if err != nil {
		return err
	}
	fake := fakeanthropic.New(fakeanthropic.Options{
		RecordDir:   recordDir,
		Scripts:     scripts,
		DefaultText: defaultText,
		BindingKey:  key,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	fmt.Printf("fakeanthropic listening on %s\n", ln.Addr())

	srv := &http.Server{Handler: logRequests(fake), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
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
	if dec.More() {
		return nil, fmt.Errorf("--scripts %s: trailing data after the scripts object", path)
	}
	return scripts, nil
}

// logRequests logs method, URI and status of each request: metadata only.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		//nolint:gosec // G706: %q escapes control characters, so a request cannot forge a log line.
		log.Printf("%q %q %d", r.Method, r.URL.RequestURI(), sw.status)
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
