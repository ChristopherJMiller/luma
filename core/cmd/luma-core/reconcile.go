package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ChristopherJMiller/luma/core/internal/reconcile"
)

const defaultAnthropicAPIURL = "https://api.anthropic.com"

// runUsageReconcile is `luma-core usage-reconcile [--day YYYY-MM-DD]` (P22):
// the CronJob that posts Anthropic's per-workspace daily cost to Core. The
// default day is yesterday, UTC. The key file and token file are read per run;
// neither is logged. now is injected for tests.
func runUsageReconcile(ctx context.Context, args []string, env func(string) string, now func() time.Time) error {
	fs := flag.NewFlagSet("usage-reconcile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dayFlag := fs.String("day", "", "the UTC day to reconcile, YYYY-MM-DD (default yesterday)")
	if err := parse(fs, args); err != nil {
		return err
	}
	day := now().UTC().AddDate(0, 0, -1)
	if *dayFlag != "" {
		d, err := time.Parse("2006-01-02", *dayFlag)
		if err != nil {
			return fmt.Errorf("%w: --day must be YYYY-MM-DD", errUsage)
		}
		day = d
	}
	keyFile, coreURL, tokenFile := env("LUMA_ANTHROPIC_ADMIN_KEY_FILE"), env("LUMA_CORE_HTTP_URL"), env("LUMA_CORE_TOKEN_FILE")
	var names []string
	for _, n := range strings.Split(env("LUMA_RECONCILE_WORKSPACES"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	switch {
	case keyFile == "":
		return fmt.Errorf("%w: LUMA_ANTHROPIC_ADMIN_KEY_FILE is not set", errUsage)
	case coreURL == "":
		return fmt.Errorf("%w: LUMA_CORE_HTTP_URL is not set", errUsage)
	case tokenFile == "":
		return fmt.Errorf("%w: LUMA_CORE_TOKEN_FILE is not set", errUsage)
	case len(names) == 0:
		return fmt.Errorf("%w: LUMA_RECONCILE_WORKSPACES is not set", errUsage)
	}
	apiURL := env("LUMA_ANTHROPIC_API_URL")
	if apiURL == "" {
		apiURL = defaultAnthropicAPIURL
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	admin := &reconcile.AdminClient{BaseURL: apiURL, KeyFile: keyFile, HTTP: hc}
	return reconcile.Run(ctx, admin, hc, coreURL, tokenFile, day, names)
}
