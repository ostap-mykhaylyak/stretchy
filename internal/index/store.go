package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
)

// Store manages all indices under a data directory.
type Store struct {
	mu      sync.RWMutex
	dir     string
	indices map[string]*Index
	log     *logx.Logger
}

func OpenStore(dir string, log *logx.Logger) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, indices: map[string]*Index{}, log: log}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		ix := newIndex(name, filepath.Join(dir, name), log)
		if err := ix.load(); err != nil {
			log.Error("load index %s: %v", name, err)
			continue
		}
		s.indices[name] = ix
		log.Info("loaded index %s (%d docs)", name, ix.DocCount())
	}
	return s, nil
}

var ErrIndexExists = fmt.Errorf("index already exists")
var ErrIndexMissing = fmt.Errorf("index not found")

// Create makes a new index with optional settings and mappings bodies.
func (s *Store) Create(name string, settings, mappings json.RawMessage) (*Index, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid index name %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.indices[name]; ok {
		return nil, ErrIndexExists
	}
	dir := filepath.Join(s.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ix := newIndex(name, dir, s.log)
	if len(settings) > 0 {
		ix.settings = settings
	}
	if len(mappings) > 0 {
		if err := ix.Mapping.UnmarshalBody(mappings); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("invalid mappings: %w", err)
		}
	}
	if err := ix.saveMetaLocked(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := ix.openWALLocked(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s.indices[name] = ix
	s.log.Info("created index %s", name)
	return ix, nil
}

// GetOrCreate returns the index, creating it on demand (Elasticsearch
// auto-creates indices on first document).
func (s *Store) GetOrCreate(name string) (*Index, error) {
	if ix, ok := s.Get(name); ok {
		return ix, nil
	}
	ix, err := s.Create(name, nil, nil)
	if err == ErrIndexExists {
		got, _ := s.Get(name)
		return got, nil
	}
	return ix, err
}

func (s *Store) Get(name string) (*Index, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ix, ok := s.indices[name]
	return ix, ok
}

func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ix, ok := s.indices[name]
	if !ok {
		return ErrIndexMissing
	}
	ix.close()
	delete(s.indices, name)
	if err := os.RemoveAll(filepath.Join(s.dir, name)); err != nil {
		return err
	}
	s.log.Info("deleted index %s", name)
	return nil
}

func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.indices))
	for n := range s.indices {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve expands a comma-separated index expression with "*"
// wildcards ("_all" and "*" match everything) into real indices.
func (s *Store) Resolve(expr string) []*Index {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Index
	seen := map[string]bool{}
	add := func(ix *Index) {
		if !seen[ix.Name] {
			seen[ix.Name] = true
			out = append(out, ix)
		}
	}
	for _, part := range splitComma(expr) {
		if part == "_all" || part == "*" {
			for _, ix := range s.indices {
				add(ix)
			}
			continue
		}
		if ix, ok := s.indices[part]; ok {
			add(ix)
			continue
		}
		// wildcard match
		for name, ix := range s.indices {
			if wildcardMatch(part, name) {
				add(ix)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// wildcardMatch supports "*" globs, e.g. "site-post-*".
func wildcardMatch(pattern, s string) bool {
	ok, err := filepath.Match(pattern, s)
	return err == nil && ok
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ix := range s.indices {
		ix.close()
	}
}
