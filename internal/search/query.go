// Package search evaluates the subset of the Elasticsearch query DSL
// that WordPress/WooCommerce integrations (ElasticPress & co.) rely on.
package search

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/ostap-mykhaylyak/stretchy/internal/analysis"
	"github.com/ostap-mykhaylyak/stretchy/internal/index"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// scores maps docID -> relevance score.
type scores map[string]float64

// Matches evaluates a raw query body against one index.
// A nil/empty query means match_all.
func Matches(ix *index.Index, rawQuery json.RawMessage) (scores, error) {
	if len(rawQuery) == 0 {
		return evalMatchAll(ix, 1), nil
	}
	var q map[string]interface{}
	if err := decodeUseNumber(rawQuery, &q); err != nil {
		return nil, fmt.Errorf("invalid query: %w", err)
	}
	return evalQuery(ix, q)
}

func evalQuery(ix *index.Index, q map[string]interface{}) (scores, error) {
	if len(q) == 0 {
		return evalMatchAll(ix, 1), nil
	}
	for kind, body := range q {
		switch kind {
		case "match_all":
			boost := getFloat(asMap(body), "boost", 1)
			return evalMatchAll(ix, boost), nil
		case "match_none":
			return scores{}, nil
		case "match":
			return evalMatch(ix, asMap(body), false)
		case "match_phrase":
			return evalMatchPhrase(ix, asMap(body), false)
		case "match_phrase_prefix":
			return evalMatchPhrase(ix, asMap(body), true)
		case "multi_match":
			return evalMultiMatch(ix, asMap(body))
		case "term":
			return evalTerm(ix, asMap(body))
		case "terms":
			return evalTerms(ix, asMap(body))
		case "range":
			return evalRange(ix, asMap(body))
		case "exists":
			return evalExists(ix, asMap(body))
		case "prefix":
			return evalPrefixWildcard(ix, asMap(body), false)
		case "wildcard":
			return evalPrefixWildcard(ix, asMap(body), true)
		case "fuzzy":
			return evalFuzzy(ix, asMap(body))
		case "ids":
			return evalIDs(ix, asMap(body))
		case "bool":
			return evalBool(ix, asMap(body))
		case "constant_score":
			return evalConstantScore(ix, asMap(body))
		case "function_score":
			return evalFunctionScore(ix, asMap(body))
		case "dis_max":
			return evalDisMax(ix, asMap(body))
		case "nested":
			// Sources are flattened, so the inner query already uses
			// full dot-paths; per-object correlation is approximated.
			inner := asMap(asMap(body)["query"])
			return evalQuery(ix, inner)
		case "query_string", "simple_query_string":
			return evalQueryString(ix, asMap(body))
		default:
			return nil, fmt.Errorf("unsupported query type %q", kind)
		}
	}
	return scores{}, nil
}

// --- leaf queries ---------------------------------------------------

func evalMatchAll(ix *index.Index, boost float64) scores {
	out := scores{}
	ix.EachDoc(func(id string, _ map[string][]interface{}) bool {
		out[id] = boost
		return true
	})
	return out
}

// fieldQueryOpts is the {field: {...}} or {field: value} shorthand.
func fieldAndOpts(body map[string]interface{}) (string, interface{}, map[string]interface{}) {
	for field, v := range body {
		if field == "boost" || field == "_name" {
			continue
		}
		if m, ok := v.(map[string]interface{}); ok {
			return field, nil, m
		}
		return field, v, nil
	}
	return "", nil, nil
}

func evalMatch(ix *index.Index, body map[string]interface{}, phrase bool) (scores, error) {
	field, val, opts := fieldAndOpts(body)
	if field == "" {
		return nil, fmt.Errorf("match: missing field")
	}
	queryText := ""
	operator := "or"
	fuzziness := ""
	boost := 1.0
	if opts != nil {
		queryText = toQueryString(opts["query"])
		if s, ok := opts["operator"].(string); ok {
			operator = strings.ToLower(s)
		}
		fuzziness = toQueryString(opts["fuzziness"])
		boost = getFloat(opts, "boost", 1)
	} else {
		queryText = toQueryString(val)
	}
	return matchField(ix, field, queryText, operator, fuzziness, boost), nil
}

// matchField scores an analyzed match of queryText against one field.
func matchField(ix *index.Index, field, queryText, operator, fuzziness string, boost float64) scores {
	field = resolveSearchField(ix, field)
	terms := analysis.Terms(queryText)
	if len(terms) == 0 {
		return scores{}
	}
	if ix.Mapping.FieldType(field) == index.TypeKeyword {
		// keyword fields match the whole value verbatim (folded)
		return keywordTermScores(ix, field, queryText, boost)
	}

	perTerm := make([]scores, 0, len(terms))
	for _, term := range terms {
		s := termScores(ix, field, term, boost)
		if fuzziness != "" && fuzziness != "0" {
			maxDist := fuzzyDistance(fuzziness, term)
			if maxDist > 0 {
				for _, cand := range ix.TermsOfField(field) {
					if cand == term {
						continue
					}
					if d := levenshteinMax(term, cand, maxDist); d > 0 && d <= maxDist {
						for id, sc := range termScores(ix, field, cand, boost) {
							v := sc * (1 - 0.25*float64(d))
							if v > s[id] {
								if s == nil {
									s = scores{}
								}
								s[id] = v
							}
						}
					}
				}
			}
		}
		perTerm = append(perTerm, s)
	}
	if operator == "and" {
		return intersectSum(perTerm)
	}
	return unionSum(perTerm)
}

// termScores computes BM25 for one exact term.
func termScores(ix *index.Index, field, term string, boost float64) scores {
	hits := ix.PostingDocs(field, term)
	if len(hits) == 0 {
		return scores{}
	}
	nDocs, avgLen := ix.FieldStats(field)
	if nDocs == 0 {
		return scores{}
	}
	df := float64(len(hits))
	idf := math.Log(1 + (float64(nDocs)-df+0.5)/(df+0.5))
	out := make(scores, len(hits))
	for _, h := range hits {
		tf := float64(h.Freq)
		dl := float64(ix.FieldLen(field, h.ID))
		denom := tf + bm25K1*(1-bm25B+bm25B*dl/math.Max(avgLen, 1))
		out[h.ID] = boost * idf * (tf * (bm25K1 + 1) / denom)
	}
	return out
}

func keywordTermScores(ix *index.Index, field, value string, boost float64) scores {
	out := scores{}
	for _, h := range ix.PostingDocs(field, value) {
		out[h.ID] = boost
	}
	if len(out) == 0 {
		// tolerate case differences for convenience
		want := analysis.Normalize(value)
		for _, cand := range ix.TermsOfField(field) {
			if analysis.Normalize(cand) == want {
				for _, h := range ix.PostingDocs(field, cand) {
					out[h.ID] = boost
				}
			}
		}
	}
	return out
}

func evalMatchPhrase(ix *index.Index, body map[string]interface{}, prefix bool) (scores, error) {
	field, val, opts := fieldAndOpts(body)
	if field == "" {
		return nil, fmt.Errorf("match_phrase: missing field")
	}
	queryText := toQueryString(val)
	slop := 0
	boost := 1.0
	if opts != nil {
		queryText = toQueryString(opts["query"])
		slop = int(getFloat(opts, "slop", 0))
		boost = getFloat(opts, "boost", 1)
	}
	field = resolveSearchField(ix, field)
	terms := analysis.Terms(queryText)
	if len(terms) == 0 {
		return scores{}, nil
	}
	if len(terms) == 1 && !prefix {
		return termScores(ix, field, terms[0], boost), nil
	}

	lastIdx := len(terms) - 1
	// positions per doc per term
	type docPos map[string][]int
	perTerm := make([]docPos, len(terms))
	for i, term := range terms {
		dp := docPos{}
		if prefix && i == lastIdx {
			for _, cand := range ix.TermsOfField(field) {
				if strings.HasPrefix(cand, term) {
					for _, h := range ix.PostingDocs(field, cand) {
						dp[h.ID] = append(dp[h.ID], h.Pos...)
					}
				}
			}
		} else {
			for _, h := range ix.PostingDocs(field, term) {
				dp[h.ID] = h.Pos
			}
		}
		perTerm[i] = dp
	}

	out := scores{}
	for id, firstPos := range perTerm[0] {
		ok := false
		for _, p := range firstPos {
			match := true
			prev := p
			for i := 1; i < len(terms); i++ {
				found := false
				for _, q := range perTerm[i][id] {
					if q > prev && q <= prev+1+slop {
						prev = q
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				ok = true
				break
			}
		}
		if ok {
			base := termScores(ix, field, terms[0], boost)[id]
			if base == 0 {
				base = boost
			}
			out[id] = base * float64(len(terms))
		}
	}
	return out, nil
}

func evalMultiMatch(ix *index.Index, body map[string]interface{}) (scores, error) {
	queryText := toQueryString(body["query"])
	operator := "or"
	if s, ok := body["operator"].(string); ok {
		operator = strings.ToLower(s)
	}
	fuzziness := toQueryString(body["fuzziness"])
	mmType := "best_fields"
	if s, ok := body["type"].(string); ok {
		mmType = s
	}
	boost := getFloat(body, "boost", 1)

	var fields []string
	if arr, ok := body["fields"].([]interface{}); ok {
		for _, f := range arr {
			fields = append(fields, toQueryString(f))
		}
	}
	if len(fields) == 0 {
		for path, t := range ix.Mapping.LeafFields() {
			if t == index.TypeText {
				fields = append(fields, path)
			}
		}
	}

	var perField []scores
	for _, spec := range fields {
		field, fboost := parseFieldBoost(spec)
		var s scores
		if mmType == "phrase" {
			s, _ = evalMatchPhrase(ix, map[string]interface{}{
				field: map[string]interface{}{"query": queryText, "boost": fboost * boost},
			}, false)
		} else {
			s = matchField(ix, field, queryText, operator, fuzziness, fboost*boost)
		}
		perField = append(perField, s)
	}
	if mmType == "most_fields" {
		return unionSum(perField), nil
	}
	return unionMax(perField), nil // best_fields, cross_fields approx
}

func parseFieldBoost(spec string) (string, float64) {
	if i := strings.LastIndex(spec, "^"); i > 0 {
		var b float64
		if _, err := fmt.Sscanf(spec[i+1:], "%g", &b); err == nil && b > 0 {
			return spec[:i], b
		}
	}
	return spec, 1
}

func evalTerm(ix *index.Index, body map[string]interface{}) (scores, error) {
	field, val, opts := fieldAndOpts(body)
	if field == "" {
		return nil, fmt.Errorf("term: missing field")
	}
	boost := 1.0
	if opts != nil {
		val = opts["value"]
		if val == nil {
			val = opts["term"]
		}
		boost = getFloat(opts, "boost", 1)
	}
	return termFilter(ix, field, val, boost), nil
}

func evalTerms(ix *index.Index, body map[string]interface{}) (scores, error) {
	for field, v := range body {
		if field == "boost" {
			continue
		}
		arr, ok := v.([]interface{})
		if !ok {
			continue
		}
		out := scores{}
		for _, val := range arr {
			for id, s := range termFilter(ix, field, val, 1) {
				if s > out[id] {
					out[id] = s
				}
			}
		}
		return out, nil
	}
	return scores{}, nil
}

// termFilter finds exact matches for a value on any field type.
func termFilter(ix *index.Index, field string, val interface{}, boost float64) scores {
	fieldType := ix.Mapping.FieldType(field)
	strVal := index.ToString(val)

	switch fieldType {
	case index.TypeKeyword:
		return keywordTermScores(ix, field, strVal, boost)
	case index.TypeText:
		out := scores{}
		term := strVal
		if hits := ix.PostingDocs(field, term); len(hits) > 0 {
			for _, h := range hits {
				out[h.ID] = boost
			}
			return out
		}
		// term queries are not analyzed in ES; folding here makes
		// term-on-text behave the way users expect
		for _, h := range ix.PostingDocs(field, analysis.Normalize(term)) {
			out[h.ID] = boost
		}
		return out
	}

	// numeric / bool / date / unmapped: scan doc values
	out := scores{}
	wantNum, wantIsNum := index.ToFloat(val)
	ix.EachDoc(func(id string, values map[string][]interface{}) bool {
		for _, dv := range values[field] {
			if wantIsNum {
				if got, ok := index.ToFloat(dv); ok && got == wantNum {
					out[id] = boost
					return true
				}
			}
			if index.ToString(dv) == strVal {
				out[id] = boost
				return true
			}
		}
		return true
	})
	return out
}

func evalRange(ix *index.Index, body map[string]interface{}) (scores, error) {
	field, _, opts := fieldAndOpts(body)
	if field == "" || opts == nil {
		return nil, fmt.Errorf("range: missing field")
	}
	boost := getFloat(opts, "boost", 1)

	type bound struct {
		num    float64
		str    string
		isNum  bool
		active bool
	}
	mkBound := func(key string) bound {
		v, ok := opts[key]
		if !ok || v == nil {
			return bound{}
		}
		b := bound{active: true, str: index.ToString(v)}
		if n, ok := index.ToFloat(v); ok {
			b.num, b.isNum = n, true
		}
		return b
	}
	gte, gt, lte, lt := mkBound("gte"), mkBound("gt"), mkBound("lte"), mkBound("lt")

	inRange := func(v interface{}) bool {
		num, isNum := index.ToFloat(v)
		str := index.ToString(v)
		cmp := func(b bound) (int, bool) {
			if !b.active {
				return 0, false
			}
			if isNum && b.isNum {
				switch {
				case num < b.num:
					return -1, true
				case num > b.num:
					return 1, true
				default:
					return 0, true
				}
			}
			return strings.Compare(str, b.str), true
		}
		if c, on := cmp(gte); on && c < 0 {
			return false
		}
		if c, on := cmp(gt); on && c <= 0 {
			return false
		}
		if c, on := cmp(lte); on && c > 0 {
			return false
		}
		if c, on := cmp(lt); on && c >= 0 {
			return false
		}
		return true
	}

	out := scores{}
	ix.EachDoc(func(id string, values map[string][]interface{}) bool {
		for _, dv := range values[field] {
			if inRange(dv) {
				out[id] = boost
				break
			}
		}
		return true
	})
	return out, nil
}

func evalExists(ix *index.Index, body map[string]interface{}) (scores, error) {
	field := toQueryString(body["field"])
	if field == "" {
		return nil, fmt.Errorf("exists: missing field")
	}
	prefix := field + "."
	out := scores{}
	ix.EachDoc(func(id string, values map[string][]interface{}) bool {
		if _, ok := values[field]; ok {
			out[id] = 1
			return true
		}
		for path := range values {
			if strings.HasPrefix(path, prefix) {
				out[id] = 1
				return true
			}
		}
		return true
	})
	return out, nil
}

func evalPrefixWildcard(ix *index.Index, body map[string]interface{}, wildcard bool) (scores, error) {
	field, val, opts := fieldAndOpts(body)
	if field == "" {
		return nil, fmt.Errorf("prefix/wildcard: missing field")
	}
	boost := 1.0
	if opts != nil {
		val = opts["value"]
		if val == nil {
			val = opts["wildcard"]
		}
		if val == nil {
			val = opts["prefix"]
		}
		boost = getFloat(opts, "boost", 1)
	}
	pattern := index.ToString(val)
	field = resolveTermField(ix, field)

	matchTerm := func(term string) bool {
		if wildcard {
			return wildcardMatchString(pattern, term) ||
				wildcardMatchString(analysis.Normalize(pattern), analysis.Normalize(term))
		}
		return strings.HasPrefix(term, pattern) ||
			strings.HasPrefix(analysis.Normalize(term), analysis.Normalize(pattern))
	}

	out := scores{}
	fieldType := ix.Mapping.FieldType(field)
	if fieldType == index.TypeText || fieldType == index.TypeKeyword {
		for _, term := range ix.TermsOfField(field) {
			if matchTerm(term) {
				for _, h := range ix.PostingDocs(field, term) {
					out[h.ID] = boost
				}
			}
		}
		return out, nil
	}
	ix.EachDoc(func(id string, values map[string][]interface{}) bool {
		for _, dv := range values[field] {
			if matchTerm(index.ToString(dv)) {
				out[id] = boost
				break
			}
		}
		return true
	})
	return out, nil
}

func evalFuzzy(ix *index.Index, body map[string]interface{}) (scores, error) {
	field, val, opts := fieldAndOpts(body)
	if field == "" {
		return nil, fmt.Errorf("fuzzy: missing field")
	}
	fuzziness := "AUTO"
	boost := 1.0
	if opts != nil {
		val = opts["value"]
		if f := toQueryString(opts["fuzziness"]); f != "" {
			fuzziness = f
		}
		boost = getFloat(opts, "boost", 1)
	}
	return matchField(ix, field, index.ToString(val), "or", fuzziness, boost), nil
}

func evalIDs(ix *index.Index, body map[string]interface{}) (scores, error) {
	out := scores{}
	if arr, ok := body["values"].([]interface{}); ok {
		for _, v := range arr {
			id := index.ToString(v)
			if _, ok := ix.Get(id); ok {
				out[id] = 1
			}
		}
	}
	return out, nil
}

// --- compound queries -----------------------------------------------

func evalBool(ix *index.Index, body map[string]interface{}) (scores, error) {
	boost := getFloat(body, "boost", 1)

	clauses := func(key string) ([]map[string]interface{}, error) {
		v, ok := body[key]
		if !ok {
			return nil, nil
		}
		var list []map[string]interface{}
		switch t := v.(type) {
		case []interface{}:
			for _, c := range t {
				if m, ok := c.(map[string]interface{}); ok {
					list = append(list, m)
				}
			}
		case map[string]interface{}:
			list = append(list, t)
		default:
			return nil, fmt.Errorf("bool.%s: invalid clause", key)
		}
		return list, nil
	}

	must, err := clauses("must")
	if err != nil {
		return nil, err
	}
	filter, err := clauses("filter")
	if err != nil {
		return nil, err
	}
	should, err := clauses("should")
	if err != nil {
		return nil, err
	}
	mustNot, err := clauses("must_not")
	if err != nil {
		return nil, err
	}

	var result scores
	restricted := false // whether result is already constrained

	for _, c := range must {
		s, err := evalQuery(ix, c)
		if err != nil {
			return nil, err
		}
		result = combineAnd(result, s, restricted, true)
		restricted = true
	}
	for _, c := range filter {
		s, err := evalQuery(ix, c)
		if err != nil {
			return nil, err
		}
		result = combineAnd(result, s, restricted, false)
		restricted = true
	}

	minShould := 0
	if !restricted && len(should) > 0 {
		minShould = 1
	}
	if v, ok := body["minimum_should_match"]; ok {
		if n, ok := index.ToFloat(v); ok {
			minShould = int(n)
		}
	}

	if len(should) > 0 {
		shouldScores := make([]scores, 0, len(should))
		for _, c := range should {
			s, err := evalQuery(ix, c)
			if err != nil {
				return nil, err
			}
			shouldScores = append(shouldScores, s)
		}
		matchCount := map[string]int{}
		sumScore := scores{}
		for _, s := range shouldScores {
			for id, v := range s {
				matchCount[id]++
				sumScore[id] += v
			}
		}
		if restricted {
			for id := range result {
				if minShould > 0 && matchCount[id] < minShould {
					delete(result, id)
					continue
				}
				result[id] += sumScore[id]
			}
		} else {
			result = scores{}
			for id, n := range matchCount {
				if n >= minShould {
					result[id] = sumScore[id]
				}
			}
			restricted = true
		}
	}

	if !restricted {
		result = evalMatchAll(ix, 0)
	}

	for _, c := range mustNot {
		s, err := evalQuery(ix, c)
		if err != nil {
			return nil, err
		}
		for id := range s {
			delete(result, id)
		}
	}

	if boost != 1 {
		for id := range result {
			result[id] *= boost
		}
	}
	// docs matched only by filters keep a neutral score of 0; bump to
	// a minimal constant so they surface with match_all-like scoring
	for id, v := range result {
		if v == 0 {
			result[id] = 0.0
		}
	}
	return result, nil
}

func combineAnd(acc, s scores, restricted, addScores bool) scores {
	if !restricted {
		if !addScores {
			out := make(scores, len(s))
			for id := range s {
				out[id] = 0
			}
			return out
		}
		out := make(scores, len(s))
		for id, v := range s {
			out[id] = v
		}
		return out
	}
	for id := range acc {
		v, ok := s[id]
		if !ok {
			delete(acc, id)
			continue
		}
		if addScores {
			acc[id] += v
		}
	}
	return acc
}

func evalConstantScore(ix *index.Index, body map[string]interface{}) (scores, error) {
	boost := getFloat(body, "boost", 1)
	inner := asMap(body["filter"])
	if inner == nil {
		inner = asMap(body["query"])
	}
	s, err := evalQuery(ix, inner)
	if err != nil {
		return nil, err
	}
	out := make(scores, len(s))
	for id := range s {
		out[id] = boost
	}
	return out, nil
}

func evalDisMax(ix *index.Index, body map[string]interface{}) (scores, error) {
	tie := getFloat(body, "tie_breaker", 0)
	arr, _ := body["queries"].([]interface{})
	var all []scores
	for _, c := range arr {
		if m, ok := c.(map[string]interface{}); ok {
			s, err := evalQuery(ix, m)
			if err != nil {
				return nil, err
			}
			all = append(all, s)
		}
	}
	out := scores{}
	sum := scores{}
	for _, s := range all {
		for id, v := range s {
			if v > out[id] {
				out[id] = v
			}
			sum[id] += v
		}
	}
	if tie > 0 {
		for id := range out {
			out[id] += tie * (sum[id] - out[id])
		}
	}
	return out, nil
}

// evalFunctionScore evaluates the inner query and applies weight and
// field_value_factor functions. Decay functions (gauss/exp/linear) are
// accepted but contribute a neutral 1.0.
func evalFunctionScore(ix *index.Index, body map[string]interface{}) (scores, error) {
	inner := asMap(body["query"])
	s, err := evalQuery(ix, inner)
	if err != nil {
		return nil, err
	}
	boostMode := "multiply"
	if v, ok := body["boost_mode"].(string); ok {
		boostMode = v
	}

	type fn struct {
		filter scores
		hasFil bool
		weight float64
		fvf    map[string]interface{}
	}
	var fns []fn
	if arr, ok := body["functions"].([]interface{}); ok {
		for _, f := range arr {
			fm, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			e := fn{weight: getFloat(fm, "weight", 1)}
			if fl, ok := fm["filter"].(map[string]interface{}); ok {
				fs, err := evalQuery(ix, fl)
				if err != nil {
					return nil, err
				}
				e.filter, e.hasFil = fs, true
			}
			if fvf, ok := fm["field_value_factor"].(map[string]interface{}); ok {
				e.fvf = fvf
			}
			fns = append(fns, e)
		}
	}
	if w, ok := body["weight"]; ok {
		if n, ok := index.ToFloat(w); ok {
			fns = append(fns, fn{weight: n})
		}
	}
	if fvf, ok := body["field_value_factor"].(map[string]interface{}); ok {
		fns = append(fns, fn{weight: 1, fvf: fvf})
	}
	if len(fns) == 0 {
		return s, nil
	}

	out := make(scores, len(s))
	for id, base := range s {
		factor := 1.0
		for _, f := range fns {
			if f.hasFil {
				if _, ok := f.filter[id]; !ok {
					continue
				}
			}
			w := f.weight
			if f.fvf != nil {
				w *= fieldValueFactor(ix, id, f.fvf)
			}
			factor *= w
		}
		switch boostMode {
		case "sum":
			out[id] = base + factor
		case "replace":
			out[id] = factor
		case "max":
			out[id] = math.Max(base, factor)
		case "min":
			out[id] = math.Min(base, factor)
		case "avg":
			out[id] = (base + factor) / 2
		default:
			out[id] = base * factor
		}
	}
	return out, nil
}

func fieldValueFactor(ix *index.Index, docID string, fvf map[string]interface{}) float64 {
	field := toQueryString(fvf["field"])
	factor := getFloat(fvf, "factor", 1)
	missing := getFloat(fvf, "missing", 1)
	modifier, _ := fvf["modifier"].(string)

	v := missing
	if vals := ix.DocValues(docID)[field]; len(vals) > 0 {
		if n, ok := index.ToFloat(vals[0]); ok {
			v = n
		}
	}
	v *= factor
	switch modifier {
	case "log":
		v = math.Log10(math.Max(v, 1e-9))
	case "log1p":
		v = math.Log10(v + 1)
	case "log2p":
		v = math.Log10(v + 2)
	case "ln":
		v = math.Log(math.Max(v, 1e-9))
	case "ln1p":
		v = math.Log1p(v)
	case "ln2p":
		v = math.Log(v + 2)
	case "square":
		v = v * v
	case "sqrt":
		v = math.Sqrt(math.Max(v, 0))
	case "reciprocal":
		if v != 0 {
			v = 1 / v
		}
	}
	return v
}

func evalQueryString(ix *index.Index, body map[string]interface{}) (scores, error) {
	queryText := toQueryString(body["query"])
	// strip the operators we don't interpret
	queryText = strings.NewReplacer("AND", " ", "OR", " ", "NOT", " ", "\"", " ", "+", " ", "-", " ").Replace(queryText)
	operator := "or"
	if s, ok := body["default_operator"].(string); ok {
		operator = strings.ToLower(s)
	}
	mm := map[string]interface{}{"query": queryText, "operator": operator}
	if f, ok := body["fields"]; ok {
		mm["fields"] = f
	}
	if df, ok := body["default_field"].(string); ok && df != "" && df != "*" {
		mm["fields"] = []interface{}{df}
	}
	return evalMultiMatch(ix, mm)
}

// --- helpers --------------------------------------------------------

// resolveSearchField maps "field.keyword"-style requests onto the
// indexed representation best suited for analyzed search.
func resolveSearchField(ix *index.Index, field string) string {
	if ix.Mapping.FieldType(field) != "" {
		return field
	}
	// unmapped: maybe caller searches an object prefix; leave as-is
	return field
}

// resolveTermField prefers the keyword multi-field for exact matching
// when present.
func resolveTermField(ix *index.Index, field string) string {
	m := ix.Mapping
	if m.FieldType(field) == index.TypeText && m.FieldType(field+".keyword") == index.TypeKeyword {
		return field
	}
	return field
}

func unionSum(list []scores) scores {
	out := scores{}
	for _, s := range list {
		for id, v := range s {
			out[id] += v
		}
	}
	return out
}

func unionMax(list []scores) scores {
	out := scores{}
	for _, s := range list {
		for id, v := range s {
			if v > out[id] {
				out[id] = v
			}
		}
	}
	return out
}

func intersectSum(list []scores) scores {
	if len(list) == 0 {
		return scores{}
	}
	out := scores{}
	for id, v := range list[0] {
		out[id] = v
	}
	for _, s := range list[1:] {
		for id := range out {
			if v, ok := s[id]; ok {
				out[id] += v
			} else {
				delete(out, id)
			}
		}
	}
	return out
}

func fuzzyDistance(fuzziness, term string) int {
	switch strings.ToUpper(fuzziness) {
	case "AUTO", "AUTO:3,6":
		switch {
		case len(term) < 3:
			return 0
		case len(term) < 6:
			return 1
		default:
			return 2
		}
	case "1":
		return 1
	case "2":
		return 2
	default:
		return 0
	}
}

// levenshteinMax computes edit distance but bails out early above max.
// Returns max+1 when the distance exceeds max.
func levenshteinMax(a, b string, max int) int {
	la, lb := len(a), len(b)
	if abs(la-lb) > max {
		return max + 1
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func wildcardMatchString(pattern, s string) bool {
	// simple glob: * and ?
	var match func(p, str string) bool
	match = func(p, str string) bool {
		for len(p) > 0 {
			switch p[0] {
			case '*':
				for i := 0; i <= len(str); i++ {
					if match(p[1:], str[i:]) {
						return true
					}
				}
				return false
			case '?':
				if len(str) == 0 {
					return false
				}
				p, str = p[1:], str[1:]
			default:
				if len(str) == 0 || p[0] != str[0] {
					return false
				}
				p, str = p[1:], str[1:]
			}
		}
		return len(str) == 0
	}
	return match(pattern, s)
}

func asMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func getFloat(m map[string]interface{}, key string, def float64) float64 {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok {
		if f, ok := index.ToFloat(v); ok {
			return f
		}
	}
	return def
}

func toQueryString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return index.ToString(v)
}

func decodeUseNumber(raw json.RawMessage, dst interface{}) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(dst)
}
