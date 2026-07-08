package search

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ostap-mykhaylyak/stretchy/internal/analysis"
	"github.com/ostap-mykhaylyak/stretchy/internal/index"
)

type Request struct {
	Query          json.RawMessage            `json:"query"`
	From           int                        `json:"from"`
	Size           *int                       `json:"size"`
	Sort           json.RawMessage            `json:"sort"`
	Aggs           map[string]json.RawMessage `json:"aggs"`
	Aggregations   map[string]json.RawMessage `json:"aggregations"`
	PostFilter     json.RawMessage            `json:"post_filter"`
	Source         json.RawMessage            `json:"_source"`
	Highlight      *HighlightSpec             `json:"highlight"`
	MinScore       float64                    `json:"min_score"`
	TrackTotalHits interface{}                `json:"track_total_hits"`
}

type HighlightSpec struct {
	PreTags  []string                   `json:"pre_tags"`
	PostTags []string                   `json:"post_tags"`
	Fields   map[string]json.RawMessage `json:"fields"`
}

type Hit struct {
	Index     string
	ID        string
	Score     float64
	Sort      []interface{}
	Source    json.RawMessage
	Highlight map[string][]string
}

type Result struct {
	Total    int
	MaxScore *float64
	Hits     []Hit
	Aggs     map[string]interface{}
}

type matchedDoc struct {
	ix    *index.Index
	id    string
	score float64
}

func ParseRequest(body []byte) (*Request, error) {
	req := &Request{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, req); err != nil {
			return nil, fmt.Errorf("parse search body: %w", err)
		}
	}
	if req.Aggs == nil {
		req.Aggs = req.Aggregations
	}
	return req, nil
}

// Exec runs a search over one or more indices.
func Exec(indices []*index.Index, req *Request) (*Result, error) {
	size := 10
	if req.Size != nil {
		size = *req.Size
	}

	// 1. evaluate the query per index
	var matched []matchedDoc
	for _, ix := range indices {
		s, err := Matches(ix, req.Query)
		if err != nil {
			return nil, err
		}
		for id, sc := range s {
			if sc < req.MinScore {
				continue
			}
			matched = append(matched, matchedDoc{ix: ix, id: id, score: sc})
		}
	}

	// 2. aggregations run on the pre-post_filter set (ES semantics,
	// what faceting relies on)
	var aggs map[string]interface{}
	if len(req.Aggs) > 0 {
		var err error
		aggs, err = runAggs(indices, matched, req.Aggs)
		if err != nil {
			return nil, err
		}
	}

	// 3. post_filter narrows the hit list only
	if len(req.PostFilter) > 0 {
		filtered := matched[:0]
		perIndex := map[*index.Index]scores{}
		for _, ix := range indices {
			s, err := Matches(ix, req.PostFilter)
			if err != nil {
				return nil, err
			}
			perIndex[ix] = s
		}
		for _, m := range matched {
			if _, ok := perIndex[m.ix][m.id]; ok {
				filtered = append(filtered, m)
			}
		}
		matched = filtered
	}

	res := &Result{Total: len(matched), Aggs: aggs}

	// 4. sort
	var allKeys [][]sortKey
	sortSpecs, sortByScore := parseSort(req.Sort)
	if sortByScore {
		sort.Slice(matched, func(i, j int) bool {
			if matched[i].score != matched[j].score {
				return matched[i].score > matched[j].score
			}
			if matched[i].ix.Name != matched[j].ix.Name {
				return matched[i].ix.Name < matched[j].ix.Name
			}
			return matched[i].id < matched[j].id
		})
	} else {
		keys := make([][]sortKey, len(matched))
		for i, m := range matched {
			keys[i] = sortKeysFor(m, sortSpecs)
		}
		idx := make([]int, len(matched))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return compareSortKeys(keys[idx[a]], keys[idx[b]], matched[idx[a]], matched[idx[b]])
		})
		reordered := make([]matchedDoc, len(matched))
		reKeys := make([][]sortKey, len(matched))
		for i, j := range idx {
			reordered[i] = matched[j]
			reKeys[i] = keys[j]
		}
		matched = reordered
		allKeys = reKeys
	}

	if len(matched) > 0 && sortByScore {
		ms := matched[0].score
		res.MaxScore = &ms
	}

	// 5. paginate + build hits
	start := req.From
	if start < 0 {
		start = 0
	}
	if start > len(matched) {
		start = len(matched)
	}
	end := start + size
	if size < 0 || end > len(matched) {
		end = len(matched)
	}

	var hlTerms map[string][]string
	hlFuzzy := false
	if req.Highlight != nil && len(req.Highlight.Fields) > 0 {
		hlTerms, hlFuzzy = collectQueryTerms(req.Query)
	}

	for pos, m := range matched[start:end] {
		doc, ok := m.ix.Get(m.id)
		if !ok {
			continue
		}
		hit := Hit{Index: m.ix.Name, ID: m.id, Score: m.score}
		if allKeys != nil {
			hit.Sort = sortValues(allKeys[start+pos])
		}
		hit.Source = filterSource(doc.Source, req.Source)
		if hlTerms != nil {
			hit.Highlight = highlightDoc(m.ix, doc, req.Highlight, hlTerms, hlFuzzy)
		}
		res.Hits = append(res.Hits, hit)
	}
	return res, nil
}

// --- sorting --------------------------------------------------------

type sortSpec struct {
	field   string
	desc    bool
	missing string // "_last" (default) or "_first"
}

type sortKey struct {
	num    float64
	str    string
	isNum  bool
	isMiss bool
	desc   bool
}

// parseSort returns the specs and whether plain score ordering applies.
func parseSort(raw json.RawMessage) ([]sortSpec, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var any interface{}
	if err := json.Unmarshal(raw, &any); err != nil {
		return nil, true
	}
	var list []interface{}
	switch t := any.(type) {
	case []interface{}:
		list = t
	default:
		list = []interface{}{t}
	}
	var specs []sortSpec
	for _, item := range list {
		switch t := item.(type) {
		case string:
			field, order := t, "asc"
			if i := strings.Index(t, ":"); i > 0 {
				field, order = t[:i], t[i+1:]
			}
			if field == "_score" && order != "asc" {
				specs = append(specs, sortSpec{field: "_score", desc: true})
			} else {
				specs = append(specs, sortSpec{field: field, desc: order == "desc"})
			}
		case map[string]interface{}:
			for field, opts := range t {
				spec := sortSpec{field: field, missing: "_last"}
				if field == "_score" {
					spec.desc = true
				}
				switch o := opts.(type) {
				case string:
					spec.desc = o == "desc"
				case map[string]interface{}:
					if s, ok := o["order"].(string); ok {
						spec.desc = s == "desc"
					}
					if s, ok := o["missing"].(string); ok {
						spec.missing = s
					}
				}
				specs = append(specs, spec)
			}
		}
	}
	if len(specs) == 0 {
		return nil, true
	}
	if len(specs) == 1 && specs[0].field == "_score" && specs[0].desc {
		return nil, true
	}
	return specs, false
}

func sortKeysFor(m matchedDoc, specs []sortSpec) []sortKey {
	values := m.ix.DocValues(m.id)
	keys := make([]sortKey, len(specs))
	for i, spec := range specs {
		k := sortKey{desc: spec.desc}
		if spec.field == "_score" {
			k.num, k.isNum = m.score, true
			keys[i] = k
			continue
		}
		vals := values[spec.field]
		if len(vals) == 0 {
			// text field sort falls back to its keyword twin's source values
			if alt := strings.TrimSuffix(spec.field, ".keyword"); alt != spec.field {
				vals = values[alt]
			}
		}
		if len(vals) == 0 {
			k.isMiss = true
			if spec.missing == "_first" {
				k.isMiss = false
				if spec.desc {
					k.num, k.isNum = 1e308, true
				} else {
					k.num, k.isNum = -1e308, true
				}
			}
			keys[i] = k
			continue
		}
		// ES picks min for asc, max for desc among multi-values
		best := vals[0]
		bestNum, bestIsNum := index.ToFloat(best)
		for _, v := range vals[1:] {
			n, isNum := index.ToFloat(v)
			if bestIsNum && isNum {
				if (spec.desc && n > bestNum) || (!spec.desc && n < bestNum) {
					best, bestNum = v, n
				}
			} else {
				s1, s2 := index.ToString(v), index.ToString(best)
				if (spec.desc && s1 > s2) || (!spec.desc && s1 < s2) {
					best = v
				}
			}
		}
		if bestIsNum {
			k.num, k.isNum = bestNum, true
		} else {
			k.str = index.ToString(best)
		}
		keys[i] = k
	}
	return keys
}

func compareSortKeys(a, b []sortKey, ma, mb matchedDoc) bool {
	for i := range a {
		ka, kb := a[i], b[i]
		if ka.isMiss != kb.isMiss {
			return kb.isMiss // missing sorts last
		}
		if ka.isMiss {
			continue
		}
		var cmp int
		if ka.isNum && kb.isNum {
			switch {
			case ka.num < kb.num:
				cmp = -1
			case ka.num > kb.num:
				cmp = 1
			}
		} else {
			cmp = strings.Compare(keyString(ka), keyString(kb))
		}
		if cmp == 0 {
			continue
		}
		if ka.desc {
			return cmp > 0
		}
		return cmp < 0
	}
	if ma.ix.Name != mb.ix.Name {
		return ma.ix.Name < mb.ix.Name
	}
	return ma.id < mb.id
}

func keyString(k sortKey) string {
	if k.isNum {
		return fmt.Sprintf("%020.6f", k.num)
	}
	return k.str
}

func sortValues(keys []sortKey) []interface{} {
	out := make([]interface{}, len(keys))
	for i, k := range keys {
		switch {
		case k.isMiss:
			out[i] = nil
		case k.isNum:
			out[i] = k.num
		default:
			out[i] = k.str
		}
	}
	return out
}

// --- _source filtering ----------------------------------------------

func filterSource(source json.RawMessage, spec json.RawMessage) json.RawMessage {
	if len(spec) == 0 {
		return source
	}
	var any interface{}
	if err := json.Unmarshal(spec, &any); err != nil {
		return source
	}
	switch t := any.(type) {
	case bool:
		if !t {
			return nil
		}
		return source
	case string:
		return projectSource(source, []string{t}, nil)
	case []interface{}:
		var includes []string
		for _, v := range t {
			if s, ok := v.(string); ok {
				includes = append(includes, s)
			}
		}
		return projectSource(source, includes, nil)
	case map[string]interface{}:
		var includes, excludes []string
		collect := func(v interface{}) []string {
			var out []string
			switch s := v.(type) {
			case string:
				out = append(out, s)
			case []interface{}:
				for _, e := range s {
					if str, ok := e.(string); ok {
						out = append(out, str)
					}
				}
			}
			return out
		}
		includes = collect(t["includes"])
		excludes = collect(t["excludes"])
		return projectSource(source, includes, excludes)
	}
	return source
}

func projectSource(source json.RawMessage, includes, excludes []string) json.RawMessage {
	var doc map[string]interface{}
	if err := json.Unmarshal(source, &doc); err != nil {
		return source
	}
	match := func(patterns []string, key string) bool {
		for _, p := range patterns {
			if p == key || wildcardMatchString(p, key) || strings.HasPrefix(key, p+".") {
				return true
			}
		}
		return false
	}
	out := map[string]interface{}{}
	for k, v := range doc {
		if len(includes) > 0 && !match(includes, k) {
			continue
		}
		if len(excludes) > 0 && match(excludes, k) {
			continue
		}
		out[k] = v
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return source
	}
	return raw
}

// --- highlight ------------------------------------------------------

// collectQueryTerms walks the query and gathers analyzed terms per
// field ("*" collects terms applying to any field). The second return
// reports whether any clause asked for fuzzy matching, so the
// highlighter can honor fuzzy hits too.
func collectQueryTerms(rawQuery json.RawMessage) (map[string][]string, bool) {
	out := map[string][]string{}
	fuzzy := false
	if len(rawQuery) == 0 {
		return out, false
	}
	var q interface{}
	if err := json.Unmarshal(rawQuery, &q); err != nil {
		return out, false
	}
	var walk func(node interface{})
	walk = func(node interface{}) {
		switch t := node.(type) {
		case []interface{}:
			for _, v := range t {
				walk(v)
			}
		case map[string]interface{}:
			for key, v := range t {
				switch key {
				case "match", "match_phrase", "match_phrase_prefix", "term", "fuzzy", "prefix", "wildcard":
					if key == "fuzzy" {
						fuzzy = true
					}
					if body, ok := v.(map[string]interface{}); ok {
						for field, fv := range body {
							var text string
							switch b := fv.(type) {
							case string:
								text = b
							case map[string]interface{}:
								text = toQueryString(b["query"])
								if text == "" {
									text = toQueryString(b["value"])
								}
								if f := toQueryString(b["fuzziness"]); f != "" && f != "0" {
									fuzzy = true
								}
							default:
								text = toQueryString(fv)
							}
							base := strings.TrimSuffix(field, ".keyword")
							out[base] = append(out[base], analysis.Terms(text)...)
						}
					}
				case "multi_match", "query_string", "simple_query_string":
					if body, ok := v.(map[string]interface{}); ok {
						terms := analysis.Terms(toQueryString(body["query"]))
						out["*"] = append(out["*"], terms...)
						if f := toQueryString(body["fuzziness"]); f != "" && f != "0" {
							fuzzy = true
						}
					}
				default:
					walk(v)
				}
			}
		}
	}
	walk(q)
	return out, fuzzy
}

func highlightDoc(ix *index.Index, doc *index.Doc, spec *HighlightSpec, terms map[string][]string, fuzzy bool) map[string][]string {
	pre, post := "<em>", "</em>"
	if len(spec.PreTags) > 0 {
		pre = spec.PreTags[0]
	}
	if len(spec.PostTags) > 0 {
		post = spec.PostTags[0]
	}

	termSet := func(field string) map[string]bool {
		set := map[string]bool{}
		for _, t := range terms[field] {
			set[t] = true
		}
		for _, t := range terms["*"] {
			set[t] = true
		}
		return set
	}

	out := map[string][]string{}
	for reqField := range spec.Fields {
		fields := []string{reqField}
		if strings.ContainsAny(reqField, "*?") {
			fields = nil
			for path := range doc.Values {
				if wildcardMatchString(reqField, path) {
					fields = append(fields, path)
				}
			}
		}
		for _, field := range fields {
			set := termSet(strings.TrimSuffix(field, ".keyword"))
			if len(set) == 0 {
				continue
			}
			for _, v := range doc.Values[field] {
				text := index.ToString(v)
				hl, matched := highlightText(text, set, pre, post, fuzzy)
				if matched {
					out[field] = append(out[field], hl)
				}
			}
		}
	}
	return out
}

// highlightText wraps matching tokens with pre/post tags. With fuzzy
// enabled, tokens within the AUTO edit distance of a query term are
// highlighted too, mirroring how the query matched them.
func highlightText(text string, terms map[string]bool, pre, post string, fuzzy bool) (string, bool) {
	toks := analysis.Analyze(text)
	if len(toks) == 0 {
		return text, false
	}
	tokenMatches := func(term string) bool {
		if terms[term] {
			return true
		}
		if !fuzzy {
			return false
		}
		for q := range terms {
			if d := fuzzyDistance("AUTO", q); d > 0 && levenshteinMax(q, term, d) <= d {
				return true
			}
		}
		return false
	}
	// re-scan the original to find byte offsets of each token
	var b strings.Builder
	matched := false
	i := 0
	for i < len(text) {
		start, end, term := nextToken(text, i)
		if start < 0 {
			b.WriteString(text[i:])
			break
		}
		b.WriteString(text[i:start])
		if tokenMatches(term) {
			matched = true
			b.WriteString(pre)
			b.WriteString(text[start:end])
			b.WriteString(post)
		} else {
			b.WriteString(text[start:end])
		}
		i = end
	}
	return b.String(), matched
}

// nextToken finds the next alphanumeric run starting at or after i,
// returning byte offsets and the folded term.
func nextToken(text string, i int) (int, int, string) {
	start := -1
	for j, r := range text[i:] {
		alnum := isAlnum(r)
		if start < 0 {
			if alnum {
				start = i + j
			}
			continue
		}
		if !alnum {
			end := i + j
			return start, end, analysis.Normalize(text[start:end])
		}
	}
	if start < 0 {
		return -1, -1, ""
	}
	return start, len(text), analysis.Normalize(text[start:])
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127
}
