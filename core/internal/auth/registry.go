package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/ChristopherJMiller/luma/core/internal/store"
)

// Tiers a person can have (C5).
const (
	TierAdmin  = "admin"
	TierFamily = "family"
)

// registryDebounce collapses a burst of events (the kubelet's ..data swap, an
// editor's write-rename) into one reload. Tests shorten it.
var registryDebounce = 2 * time.Second

// personIDPattern keeps registry ids safe as metric label values and log
// fields (C7 labels `person` with the registry id).
var personIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// quotedValue is a value yaml.v3 quotes in an error message.
var quotedValue = regexp.MustCompile("`[^`]*`")

// Person is one registry entry as the rest of Core sees it.
type Person struct {
	ID, DisplayName, Tier string
	Household             bool
}

// Registry is the people registry (C5 `registry.yaml`): who may sign in, and
// through which connector each address is accepted. It is immutable; a reload
// builds a new one.
type Registry struct {
	people map[string]Person
	// byAddress maps a (connector id, ASCII-folded address) pair to a person
	// id. An address matches only under the connector it is listed with.
	byAddress map[address]string
}

type address struct{ connector, email string }

type registryFile struct {
	People map[string]registryPerson `yaml:"people"`
}

type registryPerson struct {
	DisplayName string              `yaml:"display_name"`
	Tier        string              `yaml:"tier"`
	Household   bool                `yaml:"household"`
	Emails      map[string][]string `yaml:"emails"`
}

// LoadRegistry reads and validates a registry file. Unknown keys, a bad tier,
// a malformed or duplicated address, or no people at all are errors: a
// registry that admits nobody is refused, so a truncated or empty mount can
// never sign every household member out.
func LoadRegistry(path string) (*Registry, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is Core's own config (LUMA_REGISTRY_FILE)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	r, err := parseRegistry(b)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	return r, nil
}

func parseRegistry(b []byte) (*Registry, error) {
	var f registryFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		// yaml.v3 quotes the offending value in backticks, which here can be
		// an address; the error is logged, so it keeps only the shape.
		return nil, errors.New(quotedValue.ReplaceAllString(err.Error(), "`…`"))
	}
	if len(f.People) == 0 {
		return nil, errors.New("no people")
	}
	r := &Registry{people: map[string]Person{}, byAddress: map[address]string{}}
	ids := make([]string, 0, len(f.People))
	for id := range f.People {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic duplicate errors
	for _, id := range ids {
		p := f.People[id]
		if !personIDPattern.MatchString(id) {
			return nil, fmt.Errorf("person id %q: want %s", id, personIDPattern)
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			return nil, fmt.Errorf("person %s: display_name is required", id)
		}
		if p.Tier != TierAdmin && p.Tier != TierFamily {
			return nil, fmt.Errorf("person %s: tier %q: want admin or family", id, p.Tier)
		}
		r.people[id] = Person{ID: id, DisplayName: p.DisplayName, Tier: p.Tier, Household: p.Household}
		for connector, emails := range p.Emails {
			if connector == "" || strings.TrimSpace(connector) != connector {
				return nil, fmt.Errorf("person %s: connector id %q", id, connector)
			}
			for i, e := range emails {
				if !plausibleAddress(e) {
					return nil, fmt.Errorf("person %s: emails.%s[%d] is not an address", id, connector, i)
				}
				k := address{connector, foldEmail(e)}
				if other, dup := r.byAddress[k]; dup {
					return nil, fmt.Errorf("person %s: emails.%s[%d] is already listed (person %s)", id, connector, i, other)
				}
				r.byAddress[k] = id
			}
		}
	}
	return r, nil
}

// plausibleAddress is a shape check, not validation: one @, both sides
// non-empty, no whitespace. The IdP verified the address itself.
func plausibleAddress(e string) bool {
	local, domain, ok := strings.Cut(e, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsFunc(e, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// foldEmail lower-cases ASCII letters only. Unicode case folding would let a
// look-alike (U+212A KELVIN SIGN folds to "k") match a listed address.
func foldEmail(e string) string {
	b := []byte(e)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Lookup finds the person an address belongs to, through one connector. The
// address is matched ASCII-case-insensitively, and only among the addresses
// listed under connectorID; an empty connector id matches nobody.
func (r *Registry) Lookup(connectorID, email string) (Person, bool) {
	if connectorID == "" || email == "" {
		return Person{}, false
	}
	id, ok := r.byAddress[address{connectorID, foldEmail(email)}]
	if !ok {
		return Person{}, false
	}
	return r.people[id], true
}

// Get returns a person by registry id.
func (r *Registry) Get(id string) (Person, bool) {
	p, ok := r.people[id]
	return p, ok
}

// admits reports whether a web session is still good under this registry:
// its connector and address still resolve to the person it was issued for.
// It returns that person as the registry now describes them (a tier change
// applies at once).
func (r *Registry) admits(w store.WebSession) (Person, bool) {
	p, ok := r.Lookup(w.ConnectorID, w.Email)
	return p, ok && p.ID == w.PersonID
}

// WatchRegistry calls onChange with a fresh registry whenever the file's
// directory changes and the file loads cleanly. A bad file keeps the old
// registry (onChange is not called) and is logged. The directory is watched,
// not the file, because the kubelet updates a Secret mount by swapping the
// ..data symlink. Once the watch is registered it reloads once, so a change
// between the caller's LoadRegistry and the registration is not lost. It
// blocks until ctx is done (nil), and returns an error if the watcher fails.
func WatchRegistry(ctx context.Context, path string, onChange func(*Registry)) error {
	return watchRegistry(ctx, path, func(r *Registry, err error) {
		if err != nil {
			slog.Warn("auth: registry reload failed, keeping the previous registry", "err", err)
			return
		}
		onChange(r)
	})
}

// watchRegistry is WatchRegistry with every reload's outcome reported.
func watchRegistry(ctx context.Context, path string, onReload func(*Registry, error)) error {
	dir := filepath.Dir(path)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	reload := func() {
		r, err := LoadRegistry(path)
		if err == nil {
			slog.Info("auth: registry reloaded", "people", len(r.people))
		}
		onReload(r, err)
	}
	reload()

	timer := time.NewTimer(registryDebounce)
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
				return errors.New("registry watcher: events channel closed")
			}
			timer.Reset(registryDebounce)
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("registry watcher: errors channel closed")
			}
			slog.Warn("auth: registry watcher error", "dir", dir, "err", err)
		case <-timer.C:
			reload()
		}
	}
}
