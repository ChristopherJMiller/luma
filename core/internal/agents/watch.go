package agents

import (
	"context"
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
// It blocks until ctx is done. The initial load is the caller's: Watch calls
// onChange only after a change.
func Watch(ctx context.Context, dir string, onChange func(*Catalog)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	defer func() { _ = w.Close() }()
	// The kubelet swaps the ..data symlink in the mount directory itself, so
	// the directory is what to watch. One level of agent subdirectories (the
	// <id>/AGENT.md layout) is watched too.
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

	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-w.Events:
			if !ok {
				return nil
			}
			timer.Reset(debounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			slog.Warn("agents: watcher error", "dir", dir, "err", err)
		case <-timer.C:
			reload(dir, onChange)
		}
	}
}

// reload loads dir and hands a good catalog to onChange. A bad one is logged
// and dropped, so the previous catalog stays in force.
func reload(dir string, onChange func(*Catalog)) {
	c, err := LoadCatalog(dir)
	if err != nil {
		slog.Warn("agents: reload failed, keeping the previous catalog", "dir", dir, "err", err)
		return
	}
	slog.Info("agents: catalog reloaded", "agents", len(c.agents))
	onChange(c)
}

// Watch reloads dir into the live catalog: Watch(ctx, dir, l.store).
func (l *Live) Watch(ctx context.Context, dir string) error {
	return Watch(ctx, dir, l.store)
}
