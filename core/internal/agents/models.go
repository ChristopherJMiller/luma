package agents

import (
	"errors"
	"fmt"
	"slices"
)

// harnessSDK is the only harness in phase 1.
const harnessSDK = "claude-agent-sdk"

// Capabilities are what a tier's model can do; an agent's requires must fit.
type Capabilities struct {
	ToolsMax int  `yaml:"tools_max"`
	Vision   bool `yaml:"vision"`
}

// Tier is one models.yaml tier binding.
type Tier struct {
	Harness           string       `yaml:"harness"`
	Model             string       `yaml:"model"`
	Effort            string       `yaml:"effort"`
	CacheTTL          string       `yaml:"cache_ttl"`
	AutoCompactWindow int          `yaml:"auto_compact_window"`
	AutoCompactPct    int          `yaml:"auto_compact_pct"`
	PDFMaxPages       int          `yaml:"pdf_max_pages"`
	Capabilities      Capabilities `yaml:"capabilities"`
}

// Models is the site's models.yaml.
type Models struct {
	Version     int             `yaml:"version"`
	HelperModel string          `yaml:"helper_model"`
	Tiers       map[string]Tier `yaml:"tiers"`
}

// ParseModels parses models.yaml strictly.
func ParseModels(b []byte) (Models, error) {
	var m Models
	if err := decodeStrict(b, &m); err != nil {
		return Models{}, fmt.Errorf("models.yaml: %w", err)
	}
	if m.Version != 1 {
		return Models{}, fmt.Errorf("models.yaml: version %d is not supported (want 1)", m.Version)
	}
	if m.HelperModel == "" {
		return Models{}, errors.New("models.yaml: helper_model is required")
	}
	for name, t := range m.Tiers {
		if !slices.Contains(validTiers, name) {
			return Models{}, fmt.Errorf("models.yaml: tier %q must be fast, smart or deep", name)
		}
		if t.Harness != harnessSDK {
			return Models{}, fmt.Errorf("models.yaml: tier %s: harness %q is not available in phase 1 (only %s)", name, t.Harness, harnessSDK)
		}
		if t.Model == "" {
			return Models{}, fmt.Errorf("models.yaml: tier %s: model is required", name)
		}
		if !slices.Contains(validEfforts, t.Effort) {
			return Models{}, fmt.Errorf("models.yaml: tier %s: effort %q must be low, medium or high", name, t.Effort)
		}
		if t.CacheTTL != "5m" && t.CacheTTL != "1h" {
			return Models{}, fmt.Errorf("models.yaml: tier %s: cache_ttl %q must be 5m or 1h", name, t.CacheTTL)
		}
		if t.PDFMaxPages <= 0 {
			return Models{}, fmt.Errorf("models.yaml: tier %s: pdf_max_pages must be positive", name)
		}
		if t.AutoCompactWindow < 0 || t.AutoCompactPct < 0 || t.AutoCompactPct > 100 {
			return Models{}, fmt.Errorf("models.yaml: tier %s: auto_compact_window/pct out of range", name)
		}
	}
	return m, nil
}
