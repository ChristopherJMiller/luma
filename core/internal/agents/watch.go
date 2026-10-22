package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounce collapses a burst of events (the kubelet's ..data swap, an editor's
// write-rename) into one reload.
const debounce = 2 * time.Second

// Watch calls onChange with a fresh catalog whenever dir changes and the
// result loads cleanly. A bad reload keeps the old catalog and is logged.
// Once the watches are registered it reloads once, so a change between the
// caller's LoadCatalog and the registration is not lost. It blocks until ctx
// is done (nil), and returns an error if the watcher fails or closes.
func Watch(ctx context.Context, dir string, onChange func(*Catalog)) error {
	return watch(ctx, dir, func(c *Catalog, err error) {
		if err != nil {
			slog.Warn("agents: reload failed, keeping the previous catalog", "dir", dir, "err", err)
			return
		}
		onChange(c)
	})
}

// watch is Watch with the outcome of every reload (a catalog, or an error)
// reported to onReload.
func watch(ctx context.Context, dir string, onReload func(*Catalog, error)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	defer func() { _ = w.Close() }()
	// The kubelet swaps the ..data symlink in the mount directory itself, so
	// the directory is what to watch. Agent subdirectories (the <id>/AGENT.md
	// layout) are watched too, including ones created later.
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				_ = w.Add(filepath.Join(dir, e.Name()))
			}
		}
	}
	doReload := func() {
		c, err := LoadCatalog(dir)
		if err == nil {
			slog.Info("agents: catalog reloaded", "agents", len(c.agents))
		}
		onReload(c, err)
	}
	doReload() // covers the gap between the caller's load and the registrations above

	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("agents watcher: events channel closed")
			}
			if ev.Has(fsnotify.Create) && !strings.HasPrefix(filepath.Base(ev.Name), ".") {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = w.Add(ev.Name)
				}
			}
			timer.Reset(debounce)
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("agents watcher: errors channel closed")
			}
			slog.Warn("agents: watcher error", "dir", dir, "err", err)
		case <-timer.C:
			doReload()
		}
	}
}
