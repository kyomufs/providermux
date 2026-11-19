// Package subscriptions holds the list the exit's node pool is built from.
//
// The list is the gateway's own file, not the tool's: the panel is the only
// editor, and the tool is handed a rendering of it on every run. Otherwise the
// operator would have two places to change a subscription and no way to tell
// which one won.
package subscriptions

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Modes of duplicate removal, as the tool spells them.
const (
	ModeStrict = "strict"
	ModeHost   = "host"
	ModeNone   = "none"
)

// maxItems bounds the list. A subscription is a handful of URLs; a store with
// hundreds of entries is a mistake or a script, and every one of them is
// fetched and probed on each refresh.
const maxItems = 32

// Item is one subscription.
type Item struct {
	// ID names the source in the tool's report and in the panel.
	ID string `json:"id"`
	// URL is the subscription address. It is a secret: most of them carry a
	// token in the path.
	URL string `json:"url"`
	// UA is the user agent the server expects. Empty means the tool's own.
	UA string `json:"ua,omitempty"`
	// Insecure skips certificate verification for this source.
	Insecure bool `json:"insecure,omitempty"`
	// Headers are extra request headers the server wants.
	Headers map[string]string `json:"headers,omitempty"`
}

// File is what the store keeps on disk.
type File struct {
	// Dedupe is how the tool collapses duplicate records into one node.
	Dedupe string `json:"dedupe"`
	// Items are the subscriptions, in the order the operator arranged them.
	Items []Item `json:"items"`
}

// Store is the subscription list, safe for concurrent use.
type Store struct {
	path string

	mu   sync.Mutex
	file File
}

// Open reads the store, or starts an empty one when the file does not exist.
//
// A missing file is not an error: the panel is how the first subscription gets
// created, and refusing to start because its own file is absent would make the
// panel useless exactly once — before the first entry.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("subscriptions: path must not be empty")
	}
	s := &Store{path: path, file: File{Dedupe: ModeStrict}}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("subscriptions: reading %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(body, &s.file); err != nil {
		return nil, fmt.Errorf("subscriptions: parsing %s: %w", path, err)
	}
	if err := s.file.validate(); err != nil {
		return nil, fmt.Errorf("subscriptions: %s: %w", path, err)
	}
	return s, nil
}

// Path is where the store lives.
func (s *Store) Path() string {
	return s.path
}

// List returns a copy of the store.
func (s *Store) List() File {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := File{Dedupe: s.file.Dedupe, Items: make([]Item, len(s.file.Items))}
	for i, item := range s.file.Items {
		out.Items[i] = copyItem(item)
	}
	return out
}

// Dedupe reports how duplicates are collapsed.
func (s *Store) Dedupe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Dedupe
}

// SetDedupe changes the duplicate rule.
func (s *Store) SetDedupe(mode string) error {
	mode = strings.TrimSpace(mode)
	if !validMode(mode) {
		return fmt.Errorf("dedupe must be one of %s, %s, %s (got %q)", ModeStrict, ModeHost, ModeNone, mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.file.Dedupe
	s.file.Dedupe = mode
	if err := s.save(); err != nil {
		s.file.Dedupe = before
		return err
	}
	return nil
}

// Add appends a subscription.
func (s *Store) Add(item Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item = s.normalize(item)
	if err := item.validate(); err != nil {
		return Item{}, err
	}
	if len(s.file.Items) >= maxItems {
		return Item{}, fmt.Errorf("the list already holds %d subscriptions; remove one first", maxItems)
	}
	for _, have := range s.file.Items {
		if strings.EqualFold(have.ID, item.ID) {
			return Item{}, fmt.Errorf("a subscription with the id %q already exists", item.ID)
		}
	}
	if other := s.duplicateOf(-1, item); other != "" {
		return Item{}, fmt.Errorf("the address %s is already in the list as %q", item.URL, other)
	}
	s.file.Items = append(s.file.Items, item)
	if err := s.save(); err != nil {
		s.file.Items = s.file.Items[:len(s.file.Items)-1]
		return Item{}, err
	}
	return copyItem(item), nil
}

// Update replaces the subscription with the same id.
func (s *Store) Update(item Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item = s.normalize(item)
	if err := item.validate(); err != nil {
		return Item{}, err
	}
	for i, have := range s.file.Items {
		if !strings.EqualFold(have.ID, item.ID) {
			continue
		}
		if other := s.duplicateOf(i, item); other != "" {
			return Item{}, fmt.Errorf("the address %s is already in the list as %q", item.URL, other)
		}
		s.file.Items[i] = item
		if err := s.save(); err != nil {
			s.file.Items[i] = have
			return Item{}, err
		}
		return copyItem(item), nil
	}
	return Item{}, fmt.Errorf("no subscription with the id %q", item.ID)
}

// Remove drops the subscription with the given id.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.file.Items {
		if !strings.EqualFold(have.ID, id) {
			continue
		}
		before := append([]Item(nil), s.file.Items...)
		s.file.Items = append(s.file.Items[:i], s.file.Items[i+1:]...)
		if err := s.save(); err != nil {
			s.file.Items = before
			return err
		}
		return nil
	}
	return fmt.Errorf("no subscription with the id %q", id)
}

// Render writes the list in the shape the tool reads.
//
// The tool parses strictly — an unknown key is an error — so only its own keys
// are emitted, and every value is quoted: a URL carries tokens and characters
// YAML would otherwise reinterpret.
func (s *Store) Render() ([]byte, error) {
	file := s.List()
	var b strings.Builder
	b.WriteString("# Generated by providermux from the subscription list the panel edits.\n")
	b.WriteString("# A change here is overwritten on the next refresh.\n")
	if len(file.Items) == 0 {
		// An empty list is still a list: the tool refuses a file without the key.
		b.WriteString("sources: []\n")
		return []byte(b.String()), nil
	}
	b.WriteString("sources:\n")
	for _, item := range file.Items {
		b.WriteString("  - id: " + quote(item.ID) + "\n")
		b.WriteString("    url: " + quote(item.URL) + "\n")
		if item.UA != "" {
			b.WriteString("    ua: " + quote(item.UA) + "\n")
		}
		if item.Insecure {
			b.WriteString("    insecure: true\n")
		}
		if len(item.Headers) > 0 {
			keys := make([]string, 0, len(item.Headers))
			for k := range item.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString("    headers:\n")
			for _, k := range keys {
				b.WriteString("      " + quote(k) + ": " + quote(item.Headers[k]) + "\n")
			}
		}
	}
	return []byte(b.String()), nil
}

// normalize fills in what the operator may leave out.
func (s *Store) normalize(item Item) Item {
	item.URL = strings.TrimSpace(item.URL)
	item.UA = strings.TrimSpace(item.UA)
	item.ID = strings.TrimSpace(item.ID)
	if item.ID == "" {
		item.ID = idFromURL(item.URL)
	}
	return item
}

// duplicateOf names an existing entry with the same address, ignoring the one
// being replaced. Two subscriptions to the same URL fetch the same nodes twice.
func (s *Store) duplicateOf(skip int, item Item) string {
	for i, have := range s.file.Items {
		if i == skip {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(have.URL), item.URL) {
			return have.ID
		}
	}
	return ""
}

func (s *Store) save() error {
	if err := s.file.validate(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return fmt.Errorf("subscriptions: encoding: %w", err)
	}
	body = append(body, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("subscriptions: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".subscriptions-*.json")
	if err != nil {
		return fmt.Errorf("subscriptions: writing %s: %w", s.path, err)
	}
	name := tmp.Name()
	// The file holds subscription URLs, and a subscription URL is a credential.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("subscriptions: writing %s: %w", s.path, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("subscriptions: writing %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("subscriptions: writing %s: %w", s.path, err)
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		return fmt.Errorf("subscriptions: writing %s: %w", s.path, err)
	}
	return nil
}

func (f *File) validate() error {
	if f.Dedupe == "" {
		f.Dedupe = ModeStrict
	}
	if !validMode(f.Dedupe) {
		return fmt.Errorf("dedupe must be one of %s, %s, %s (got %q)", ModeStrict, ModeHost, ModeNone, f.Dedupe)
	}
	if len(f.Items) > maxItems {
		return fmt.Errorf("%d subscriptions is more than the %d allowed", len(f.Items), maxItems)
	}
	seen := make(map[string]string, len(f.Items))
	for _, item := range f.Items {
		if err := item.validate(); err != nil {
			return err
		}
		key := strings.ToLower(item.ID)
		if other, ok := seen[key]; ok {
			return fmt.Errorf("the id %q is used twice (%s and %s)", item.ID, other, item.URL)
		}
		seen[key] = item.URL
	}
	return nil
}

func (i Item) validate() error {
	if strings.TrimSpace(i.ID) == "" {
		return errors.New("a subscription needs an id")
	}
	parsed, err := url.Parse(i.URL)
	if err != nil {
		return fmt.Errorf("subscription %q: %w", i.ID, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("subscription %q: the address must be http or https", i.ID)
	}
	if parsed.Host == "" {
		return fmt.Errorf("subscription %q: the address has no host", i.ID)
	}
	return nil
}

func validMode(mode string) bool {
	return mode == ModeStrict || mode == ModeHost || mode == ModeNone
}

// idFromURL derives a name from the address's host, which is what the operator
// would have typed anyway.
func idFromURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return "subscription"
	}
	host := parsed.Hostname()
	return strings.ReplaceAll(host, ".", "-")
}

func copyItem(item Item) Item {
	out := item
	if len(item.Headers) > 0 {
		out.Headers = make(map[string]string, len(item.Headers))
		for k, v := range item.Headers {
			out.Headers[k] = v
		}
	}
	return out
}

// quote wraps a value so YAML reads it as one string, whatever it holds.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
