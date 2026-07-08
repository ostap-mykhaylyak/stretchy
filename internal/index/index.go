package index

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ostap-mykhaylyak/stretchy/internal/analysis"
	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
)

type Doc struct {
	ID      string
	Version int64
	SeqNo   int64
	Source  json.RawMessage
	// Values maps flattened dot-paths to their leaf values. Immutable
	// after insertion, so it may be shared with readers.
	Values map[string][]interface{}
	// terms remembers what was fed to the inverted index, per field,
	// so deletion doesn't require a full scan.
	terms map[string][]string
}

type PostEntry struct {
	Freq int
	Pos  []int
}

// PostingHit is the read-side copy of one document's posting.
type PostingHit struct {
	ID   string
	Freq int
	Pos  []int
}

type Index struct {
	mu       sync.RWMutex
	Name     string
	settings json.RawMessage
	Mapping  *Mapping

	docs     map[string]*Doc
	inv      map[string]map[string]map[string]*PostEntry // field -> term -> docID -> entry
	fieldLen map[string]map[string]int                   // field -> docID -> analyzed length
	sumLen   map[string]int64                            // field -> total analyzed length
	seq      int64

	dir    string
	wal    *os.File
	walBuf *bufio.Writer
	walOps int
	log    *logx.Logger
}

type walRecord struct {
	Op  string          `json:"op"`
	ID  string          `json:"id"`
	Src json.RawMessage `json:"src,omitempty"`
}

func newIndex(name, dir string, log *logx.Logger) *Index {
	return &Index{
		Name:     name,
		settings: json.RawMessage(`{}`),
		Mapping:  NewMapping(),
		docs:     map[string]*Doc{},
		inv:      map[string]map[string]map[string]*PostEntry{},
		fieldLen: map[string]map[string]int{},
		sumLen:   map[string]int64{},
		dir:      dir,
		log:      log,
	}
}

// --- metadata -------------------------------------------------------

type metaFile struct {
	Settings json.RawMessage `json:"settings"`
	Mappings json.RawMessage `json:"mappings"`
}

func (ix *Index) saveMetaLocked() error {
	meta := metaFile{Settings: ix.settings, Mappings: ix.Mapping.MarshalBody()}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(ix.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(ix.dir, "meta.json"))
}

func (ix *Index) SaveMeta() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.saveMetaLocked()
}

func (ix *Index) Settings() json.RawMessage {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.settings
}

func (ix *Index) SetSettings(raw json.RawMessage) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if len(raw) > 0 {
		ix.settings = raw
	}
	ix.saveMetaLocked()
}

// --- write path -----------------------------------------------------

// Put indexes a document, replacing any previous version.
// Returns "created" or "updated" and the new version.
func (ix *Index) Put(id string, source json.RawMessage) (string, int64, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	result, version, err := ix.putLocked(id, source)
	if err != nil {
		return "", 0, err
	}
	ix.appendWALLocked(walRecord{Op: "put", ID: id, Src: source})
	return result, version, nil
}

func (ix *Index) putLocked(id string, source json.RawMessage) (string, int64, error) {
	dec := json.NewDecoder(bytes.NewReader(source))
	dec.UseNumber()
	var parsed map[string]interface{}
	if err := dec.Decode(&parsed); err != nil {
		return "", 0, fmt.Errorf("invalid document body: %w", err)
	}

	result := "created"
	var prevVersion int64
	if old, ok := ix.docs[id]; ok {
		result = "updated"
		prevVersion = old.Version
		ix.removeFromInvertedLocked(old)
	}

	values := Flatten(parsed)
	doc := &Doc{
		ID:      id,
		Version: prevVersion + 1,
		Source:  source,
		Values:  values,
		terms:   map[string][]string{},
	}
	ix.seq++
	doc.SeqNo = ix.seq

	mappingChanged := false
	for path, vals := range values {
		known := ix.Mapping.FieldType(path)
		fieldType := known
		if fieldType == "" {
			fieldType = ix.Mapping.EnsureDynamic(path, vals[0])
			if fieldType != "" {
				mappingChanged = true
			}
		}
		switch fieldType {
		case TypeText:
			ix.indexTextLocked(doc, path, vals)
			if ix.Mapping.FieldType(path+".keyword") == TypeKeyword {
				ix.indexKeywordLocked(doc, path+".keyword", vals)
			}
		case TypeKeyword:
			ix.indexKeywordLocked(doc, path, vals)
		}
		// numeric/date/bool fields are served from doc values
	}

	ix.docs[id] = doc
	if mappingChanged {
		ix.saveMetaLocked()
	}
	return result, doc.Version, nil
}

func (ix *Index) indexTextLocked(doc *Doc, field string, vals []interface{}) {
	posOffset := 0
	length := 0
	for _, v := range vals {
		toks := analysis.Analyze(ToString(v))
		for _, tok := range toks {
			ix.addPostingLocked(doc, field, tok.Term, tok.Pos+posOffset)
		}
		length += len(toks)
		posOffset += len(toks) + 100 // position gap between array values
	}
	if length > 0 {
		if ix.fieldLen[field] == nil {
			ix.fieldLen[field] = map[string]int{}
		}
		ix.fieldLen[field][doc.ID] = length
		ix.sumLen[field] += int64(length)
	}
}

func (ix *Index) indexKeywordLocked(doc *Doc, field string, vals []interface{}) {
	for _, v := range vals {
		term := ToString(v)
		if len(term) > 1024 {
			continue
		}
		ix.addPostingLocked(doc, field, term, -1)
	}
	if ix.fieldLen[field] == nil {
		ix.fieldLen[field] = map[string]int{}
	}
	ix.fieldLen[field][doc.ID] = len(vals)
	ix.sumLen[field] += int64(len(vals))
}

func (ix *Index) addPostingLocked(doc *Doc, field, term string, pos int) {
	byTerm := ix.inv[field]
	if byTerm == nil {
		byTerm = map[string]map[string]*PostEntry{}
		ix.inv[field] = byTerm
	}
	byDoc := byTerm[term]
	if byDoc == nil {
		byDoc = map[string]*PostEntry{}
		byTerm[term] = byDoc
	}
	entry := byDoc[doc.ID]
	if entry == nil {
		entry = &PostEntry{}
		byDoc[doc.ID] = entry
		doc.terms[field] = append(doc.terms[field], term)
	}
	entry.Freq++
	if pos >= 0 {
		entry.Pos = append(entry.Pos, pos)
	}
}

func (ix *Index) removeFromInvertedLocked(doc *Doc) {
	for field, terms := range doc.terms {
		byTerm := ix.inv[field]
		for _, term := range terms {
			if byDoc := byTerm[term]; byDoc != nil {
				delete(byDoc, doc.ID)
				if len(byDoc) == 0 {
					delete(byTerm, term)
				}
			}
		}
	}
	for field, byDoc := range ix.fieldLen {
		if l, ok := byDoc[doc.ID]; ok {
			ix.sumLen[field] -= int64(l)
			delete(byDoc, doc.ID)
		}
	}
}

// Delete removes a document; reports whether it existed.
func (ix *Index) Delete(id string) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	doc, ok := ix.docs[id]
	if !ok {
		return false
	}
	ix.removeFromInvertedLocked(doc)
	delete(ix.docs, id)
	ix.appendWALLocked(walRecord{Op: "del", ID: id})
	return true
}

// Update merges a partial document into the stored source
// (Elasticsearch _update with "doc"). If the document is missing and
// upsert is provided, the upsert body is indexed instead.
func (ix *Index) Update(id string, partial, upsert json.RawMessage) (string, int64, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	old, exists := ix.docs[id]
	if !exists {
		if upsert == nil {
			return "", 0, ErrDocMissing
		}
		res, v, err := ix.putLocked(id, upsert)
		if err == nil {
			ix.appendWALLocked(walRecord{Op: "put", ID: id, Src: upsert})
		}
		return res, v, err
	}
	merged, err := mergeJSON(old.Source, partial)
	if err != nil {
		return "", 0, err
	}
	res, v, err := ix.putLocked(id, merged)
	if err == nil {
		ix.appendWALLocked(walRecord{Op: "put", ID: id, Src: merged})
	}
	return res, v, err
}

var ErrDocMissing = fmt.Errorf("document missing")

func mergeJSON(base, overlay json.RawMessage) (json.RawMessage, error) {
	var a, b map[string]interface{}
	da := json.NewDecoder(bytes.NewReader(base))
	da.UseNumber()
	if err := da.Decode(&a); err != nil {
		return nil, err
	}
	db := json.NewDecoder(bytes.NewReader(overlay))
	db.UseNumber()
	if err := db.Decode(&b); err != nil {
		return nil, err
	}
	merged := deepMerge(a, b)
	return json.Marshal(merged)
}

func deepMerge(dst, src map[string]interface{}) map[string]interface{} {
	for k, v := range src {
		if sv, ok := v.(map[string]interface{}); ok {
			if dv, ok := dst[k].(map[string]interface{}); ok {
				dst[k] = deepMerge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// --- read path ------------------------------------------------------

func (ix *Index) Get(id string) (*Doc, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	d, ok := ix.docs[id]
	return d, ok
}

func (ix *Index) DocCount() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
}

// EachDoc iterates all documents under a read lock. Returning false
// from fn stops the iteration.
func (ix *Index) EachDoc(fn func(id string, values map[string][]interface{}) bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for id, d := range ix.docs {
		if !fn(id, d.Values) {
			return
		}
	}
}

// PostingDocs returns a copy of the posting list for field/term.
func (ix *Index) PostingDocs(field, term string) []PostingHit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	byDoc := ix.inv[field][term]
	if len(byDoc) == 0 {
		return nil
	}
	out := make([]PostingHit, 0, len(byDoc))
	for id, e := range byDoc {
		out = append(out, PostingHit{ID: id, Freq: e.Freq, Pos: e.Pos})
	}
	return out
}

// DocFreq returns how many documents contain field/term.
func (ix *Index) DocFreq(field, term string) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.inv[field][term])
}

// TermsOfField returns the sorted term dictionary of a field, for
// prefix / wildcard / fuzzy expansion.
func (ix *Index) TermsOfField(field string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	byTerm := ix.inv[field]
	out := make([]string, 0, len(byTerm))
	for t := range byTerm {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// FieldStats returns (docs containing field, average analyzed length).
func (ix *Index) FieldStats(field string) (int, float64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := len(ix.fieldLen[field])
	if n == 0 {
		return 0, 0
	}
	return n, float64(ix.sumLen[field]) / float64(n)
}

func (ix *Index) FieldLen(field, docID string) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.fieldLen[field][docID]
}

func (ix *Index) DocValues(id string) map[string][]interface{} {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if d, ok := ix.docs[id]; ok {
		return d.Values
	}
	return nil
}

// --- persistence ----------------------------------------------------

func (ix *Index) appendWALLocked(rec walRecord) {
	if ix.walBuf == nil {
		return
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	ix.walBuf.Write(raw)
	ix.walBuf.WriteByte('\n')
	ix.walBuf.Flush()
	ix.walOps++
}

func (ix *Index) openWALLocked() error {
	f, err := os.OpenFile(filepath.Join(ix.dir, "wal.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	ix.wal = f
	ix.walBuf = bufio.NewWriter(f)
	return nil
}

func (ix *Index) load() error {
	metaPath := filepath.Join(ix.dir, "meta.json")
	if raw, err := os.ReadFile(metaPath); err == nil {
		var meta metaFile
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("corrupt meta.json in %s: %w", ix.dir, err)
		}
		if len(meta.Settings) > 0 {
			ix.settings = meta.Settings
		}
		if len(meta.Mappings) > 0 {
			if err := ix.Mapping.UnmarshalBody(meta.Mappings); err != nil {
				return err
			}
		}
	}

	walPath := filepath.Join(ix.dir, "wal.jsonl")
	if f, err := os.Open(walPath); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		ops := 0
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var rec walRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				ix.log.Warn("index %s: skipping corrupt WAL line: %v", ix.Name, err)
				continue
			}
			switch rec.Op {
			case "put":
				if _, _, err := ix.putLocked(rec.ID, rec.Src); err != nil {
					ix.log.Warn("index %s: replay %s: %v", ix.Name, rec.ID, err)
				}
			case "del":
				if d, ok := ix.docs[rec.ID]; ok {
					ix.removeFromInvertedLocked(d)
					delete(ix.docs, rec.ID)
				}
			}
			ops++
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read WAL %s: %w", walPath, err)
		}
		ix.walOps = ops
	}

	if err := ix.openWALLocked(); err != nil {
		return err
	}
	// Compact when the log carries substantially more ops than live docs.
	if ix.walOps > 2*len(ix.docs)+256 {
		return ix.compactLocked()
	}
	return nil
}

func (ix *Index) compactLocked() error {
	tmpPath := filepath.Join(ix.dir, "wal.jsonl.tmp")
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	for id, d := range ix.docs {
		raw, err := json.Marshal(walRecord{Op: "put", ID: id, Src: d.Source})
		if err != nil {
			continue
		}
		w.Write(raw)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if ix.walBuf != nil {
		ix.walBuf.Flush()
	}
	if ix.wal != nil {
		ix.wal.Close()
	}
	if err := os.Rename(tmpPath, filepath.Join(ix.dir, "wal.jsonl")); err != nil {
		return err
	}
	ix.walOps = len(ix.docs)
	return ix.openWALLocked()
}

// Compact rewrites the WAL to contain only live documents.
func (ix *Index) Compact() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.compactLocked()
}

func (ix *Index) close() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.walOps > len(ix.docs) {
		if err := ix.compactLocked(); err != nil {
			ix.log.Warn("index %s: compact on close: %v", ix.Name, err)
		}
	}
	if ix.walBuf != nil {
		ix.walBuf.Flush()
	}
	if ix.wal != nil {
		ix.wal.Sync()
		ix.wal.Close()
		ix.wal = nil
		ix.walBuf = nil
	}
}

// ValidName reports whether an index name is acceptable (and safe to
// use as a directory name).
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '.' || name[0] == '-' || name[0] == '_' {
		return false
	}
	return !strings.ContainsAny(name, `/\*?"<>| ,#:`)
}
