// Package reconcile compares Anthropic's per-workspace daily cost with the
// ledger (P22, Task 14b). The client and Run are the usage-reconcile
// subcommand, the only process that holds the Admin key; Handler and
// InitGauges are Core's side of C10.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicVersion = "2023-06-01"
	workspacesPath   = "/v1/organizations/workspaces"
	costReportPath   = "/v1/organizations/cost_report"
	maxPages         = 100 // a guard against a server that never stops paging
	maxResponseBytes = 8 << 20
)

// AdminClient reads the Admin API. The key is read from KeyFile on every
// call, sent only as x-api-key, and never logged or put in an error.
type AdminClient struct {
	BaseURL string
	KeyFile string
	HTTP    *http.Client
}

func (c *AdminClient) key() (string, error) {
	b, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return "", errors.New("admin key file unreadable")
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", errors.New("admin key file is empty")
	}
	return k, nil
}

// get fetches one page of path with q and decodes it into out. Errors carry
// the status only: an Admin API error body is never echoed.
func (c *AdminClient) get(ctx context.Context, path string, q url.Values, out any) error {
	key, err := c.key()
	if err != nil {
		return err
	}
	u := strings.TrimRight(c.BaseURL, "/") + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("bad admin API URL")
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("User-Agent", "luma-usage-reconcile")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("GET %s failed: transport error", path) // url.Error text is dropped on purpose
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("GET %s: unreadable response", path)
	}
	return nil
}

// WorkspaceIDs maps each of names to its workspace id. A name Anthropic does
// not list is an error: a silent zero would read as "no spend".
func (c *AdminClient) WorkspaceIDs(ctx context.Context, names []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	ids := map[string]string{}
	after := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, errors.New("workspaces: too many pages")
		}
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		var body struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := c.get(ctx, workspacesPath, q, &body); err != nil {
			return nil, err
		}
		for _, w := range body.Data {
			if want[w.Name] {
				ids[w.Name] = w.ID
			}
		}
		if !body.HasMore {
			break
		}
		if body.LastID == "" || body.LastID == after {
			return nil, errors.New("workspaces: paging did not advance")
		}
		after = body.LastID
	}
	for _, n := range names {
		if ids[n] == "" {
			return nil, fmt.Errorf("workspace %q not found in the organization", n)
		}
	}
	return ids, nil
}

// DailyCost returns the cost in USD of the UTC day for each workspace name in
// ids (name -> workspace id). The cost report states amounts as decimal
// strings in cents; they are summed and divided by 100. A workspace with no
// result that day costs 0.
func (c *AdminClient) DailyCost(ctx context.Context, day time.Time, ids map[string]string) (map[string]float64, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	byID := map[string]string{}
	cents := map[string]float64{}
	for name, id := range ids {
		byID[id] = name
		cents[name] = 0
	}
	pageTok := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, errors.New("cost report: too many pages")
		}
		q := url.Values{
			"starting_at":  {start.Format(time.RFC3339)},
			"ending_at":    {start.AddDate(0, 0, 1).Format(time.RFC3339)},
			"bucket_width": {"1d"},
			"group_by[]":   {"workspace_id"},
		}
		if pageTok != "" {
			q.Set("page", pageTok)
		}
		var body struct {
			Data []struct {
				Results []struct {
					Currency    string  `json:"currency"`
					Amount      string  `json:"amount"`
					WorkspaceID *string `json:"workspace_id"`
				} `json:"results"`
			} `json:"data"`
			HasMore  bool   `json:"has_more"`
			NextPage string `json:"next_page"`
		}
		if err := c.get(ctx, costReportPath, q, &body); err != nil {
			return nil, err
		}
		for _, b := range body.Data {
			for _, r := range b.Results {
				if r.WorkspaceID == nil {
					continue
				}
				name, ok := byID[*r.WorkspaceID]
				if !ok {
					continue
				}
				if r.Currency != "" && r.Currency != "USD" {
					return nil, fmt.Errorf("cost report: unexpected currency %q", r.Currency)
				}
				v, err := strconv.ParseFloat(r.Amount, 64)
				if err != nil || v < 0 {
					return nil, errors.New("cost report: unreadable amount")
				}
				cents[name] += v
			}
		}
		if !body.HasMore {
			break
		}
		if body.NextPage == "" || body.NextPage == pageTok {
			return nil, errors.New("cost report: paging did not advance")
		}
		pageTok = body.NextPage
	}
	out := make(map[string]float64, len(cents))
	for n, c := range cents {
		out[n] = c / 100
	}
	return out, nil
}
