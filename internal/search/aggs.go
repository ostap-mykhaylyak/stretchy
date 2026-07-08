package search

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ostap-mykhaylyak/stretchy/internal/index"
)

// runAggs executes the aggregation tree over the matched document set.
func runAggs(indices []*index.Index, docs []matchedDoc, aggs map[string]json.RawMessage) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for name, raw := range aggs {
		var node map[string]json.RawMessage
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, fmt.Errorf("agg %s: %w", name, err)
		}
		res, err := runAgg(indices, docs, node)
		if err != nil {
			return nil, fmt.Errorf("agg %s: %w", name, err)
		}
		out[name] = res
	}
	return out, nil
}

func runAgg(indices []*index.Index, docs []matchedDoc, node map[string]json.RawMessage) (interface{}, error) {
	// split the node into the agg body and its sub-aggregations
	var subAggs map[string]json.RawMessage
	if raw, ok := node["aggs"]; ok {
		json.Unmarshal(raw, &subAggs)
	} else if raw, ok := node["aggregations"]; ok {
		json.Unmarshal(raw, &subAggs)
	}

	for kind, raw := range node {
		if kind == "aggs" || kind == "aggregations" || kind == "meta" {
			continue
		}
		var body map[string]interface{}
		if err := decodeUseNumber(raw, &body); err != nil {
			return nil, err
		}
		switch kind {
		case "terms":
			return aggTerms(indices, docs, body, subAggs)
		case "filter":
			return aggFilter(indices, docs, raw, subAggs)
		case "filters":
			return aggFilters(indices, docs, body, subAggs)
		case "range":
			return aggRange(indices, docs, body, subAggs)
		case "histogram":
			return aggHistogram(indices, docs, body, subAggs)
		case "date_histogram":
			return aggDateHistogram(indices, docs, body, subAggs)
		case "global":
			return aggGlobal(indices, subAggs)
		case "min", "max", "avg", "sum", "stats", "value_count", "cardinality":
			return aggMetric(docs, kind, body)
		default:
			return nil, fmt.Errorf("unsupported aggregation type %q", kind)
		}
	}
	return nil, fmt.Errorf("empty aggregation")
}

func fieldValues(m matchedDoc, field string) []interface{} {
	values := m.ix.DocValues(m.id)
	if v := values[field]; len(v) > 0 {
		return v
	}
	// multi-fields like "category.keyword" share the parent's values
	if i := strings.LastIndex(field, "."); i > 0 {
		if v := values[field[:i]]; len(v) > 0 {
			return v
		}
	}
	return nil
}

func withSubAggs(indices []*index.Index, bucketDocs []matchedDoc, subAggs map[string]json.RawMessage, bucket map[string]interface{}) error {
	if len(subAggs) == 0 {
		return nil
	}
	sub, err := runAggs(indices, bucketDocs, subAggs)
	if err != nil {
		return err
	}
	for k, v := range sub {
		bucket[k] = v
	}
	return nil
}

// --- bucket aggs ----------------------------------------------------

func aggTerms(indices []*index.Index, docs []matchedDoc, body map[string]interface{}, subAggs map[string]json.RawMessage) (interface{}, error) {
	field := toQueryString(body["field"])
	size := int(getFloat(body, "size", 10))
	if size <= 0 {
		size = 10
	}

	type bucket struct {
		key    interface{}
		keyStr string
		count  int
		docs   []matchedDoc
	}
	byKey := map[string]*bucket{}
	for _, m := range docs {
		seen := map[string]bool{}
		for _, v := range fieldValues(m, field) {
			ks := index.ToString(v)
			if seen[ks] {
				continue
			}
			seen[ks] = true
			b := byKey[ks]
			if b == nil {
				var key interface{} = ks
				if n, ok := v.(json.Number); ok {
					if f, err := n.Float64(); err == nil {
						key = f
					}
				}
				if bv, ok := v.(bool); ok {
					key = bv
				}
				b = &bucket{key: key, keyStr: ks}
				byKey[ks] = b
			}
			b.count++
			b.docs = append(b.docs, m)
		}
	}

	list := make([]*bucket, 0, len(byKey))
	for _, b := range byKey {
		list = append(list, b)
	}

	orderKey, orderAsc := "_count", false
	if om, ok := body["order"].(map[string]interface{}); ok {
		for k, v := range om {
			orderKey = k
			orderAsc = toQueryString(v) == "asc"
		}
	}
	sort.Slice(list, func(i, j int) bool {
		var less bool
		if orderKey == "_key" {
			less = list[i].keyStr < list[j].keyStr
		} else {
			if list[i].count == list[j].count {
				return list[i].keyStr < list[j].keyStr
			}
			less = list[i].count < list[j].count
		}
		if orderAsc {
			return less
		}
		return !less
	})

	sumOther := 0
	if len(list) > size {
		for _, b := range list[size:] {
			sumOther += b.count
		}
		list = list[:size]
	}

	buckets := make([]map[string]interface{}, 0, len(list))
	for _, b := range list {
		bm := map[string]interface{}{"key": b.key, "doc_count": b.count}
		if err := withSubAggs(indices, b.docs, subAggs, bm); err != nil {
			return nil, err
		}
		buckets = append(buckets, bm)
	}
	return map[string]interface{}{
		"doc_count_error_upper_bound": 0,
		"sum_other_doc_count":         sumOther,
		"buckets":                     buckets,
	}, nil
}

func aggFilter(indices []*index.Index, docs []matchedDoc, rawFilter json.RawMessage, subAggs map[string]json.RawMessage) (interface{}, error) {
	perIndex := map[*index.Index]scores{}
	for _, ix := range indices {
		s, err := Matches(ix, rawFilter)
		if err != nil {
			return nil, err
		}
		perIndex[ix] = s
	}
	var kept []matchedDoc
	for _, m := range docs {
		if _, ok := perIndex[m.ix][m.id]; ok {
			kept = append(kept, m)
		}
	}
	out := map[string]interface{}{"doc_count": len(kept)}
	if err := withSubAggs(indices, kept, subAggs, out); err != nil {
		return nil, err
	}
	return out, nil
}

func aggFilters(indices []*index.Index, docs []matchedDoc, body map[string]interface{}, subAggs map[string]json.RawMessage) (interface{}, error) {
	filters, ok := body["filters"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("filters: missing filters map")
	}
	buckets := map[string]interface{}{}
	for name, f := range filters {
		raw, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		res, err := aggFilter(indices, docs, raw, subAggs)
		if err != nil {
			return nil, err
		}
		buckets[name] = res
	}
	return map[string]interface{}{"buckets": buckets}, nil
}

func aggRange(indices []*index.Index, docs []matchedDoc, body map[string]interface{}, subAggs map[string]json.RawMessage) (interface{}, error) {
	field := toQueryString(body["field"])
	rangesRaw, _ := body["ranges"].([]interface{})
	type rng struct {
		key      string
		from, to float64
		hasFrom  bool
		hasTo    bool
		docs     []matchedDoc
		count    int
	}
	var ranges []*rng
	for _, rv := range rangesRaw {
		rm, ok := rv.(map[string]interface{})
		if !ok {
			continue
		}
		r := &rng{}
		if v, ok := rm["from"]; ok && v != nil {
			r.from, _ = index.ToFloat(v)
			r.hasFrom = true
		}
		if v, ok := rm["to"]; ok && v != nil {
			r.to, _ = index.ToFloat(v)
			r.hasTo = true
		}
		if k, ok := rm["key"].(string); ok {
			r.key = k
		} else {
			from, to := "*", "*"
			if r.hasFrom {
				from = trimFloat(r.from)
			}
			if r.hasTo {
				to = trimFloat(r.to)
			}
			r.key = from + "-" + to
		}
		ranges = append(ranges, r)
	}

	for _, m := range docs {
		for _, v := range fieldValues(m, field) {
			n, ok := index.ToFloat(v)
			if !ok {
				continue
			}
			for _, r := range ranges {
				if r.hasFrom && n < r.from {
					continue
				}
				if r.hasTo && n >= r.to {
					continue
				}
				r.count++
				r.docs = append(r.docs, m)
			}
			break // first numeric value per doc, ES uses all values but keep simple
		}
	}

	buckets := make([]map[string]interface{}, 0, len(ranges))
	for _, r := range ranges {
		bm := map[string]interface{}{"key": r.key, "doc_count": r.count}
		if r.hasFrom {
			bm["from"] = r.from
		}
		if r.hasTo {
			bm["to"] = r.to
		}
		if err := withSubAggs(indices, r.docs, subAggs, bm); err != nil {
			return nil, err
		}
		buckets = append(buckets, bm)
	}
	return map[string]interface{}{"buckets": buckets}, nil
}

func trimFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%g", f)
}

func aggHistogram(indices []*index.Index, docs []matchedDoc, body map[string]interface{}, subAggs map[string]json.RawMessage) (interface{}, error) {
	field := toQueryString(body["field"])
	interval := getFloat(body, "interval", 0)
	if interval <= 0 {
		return nil, fmt.Errorf("histogram: interval must be > 0")
	}
	counts := map[float64][]matchedDoc{}
	for _, m := range docs {
		seen := map[float64]bool{}
		for _, v := range fieldValues(m, field) {
			n, ok := index.ToFloat(v)
			if !ok {
				continue
			}
			b := math.Floor(n/interval) * interval
			if !seen[b] {
				seen[b] = true
				counts[b] = append(counts[b], m)
			}
		}
	}
	keys := make([]float64, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	buckets := make([]map[string]interface{}, 0, len(keys))
	for _, k := range keys {
		bm := map[string]interface{}{"key": k, "doc_count": len(counts[k])}
		if err := withSubAggs(indices, counts[k], subAggs, bm); err != nil {
			return nil, err
		}
		buckets = append(buckets, bm)
	}
	return map[string]interface{}{"buckets": buckets}, nil
}

func aggDateHistogram(indices []*index.Index, docs []matchedDoc, body map[string]interface{}, subAggs map[string]json.RawMessage) (interface{}, error) {
	field := toQueryString(body["field"])
	interval := toQueryString(body["calendar_interval"])
	if interval == "" {
		interval = toQueryString(body["fixed_interval"])
	}
	if interval == "" {
		interval = toQueryString(body["interval"]) // legacy
	}

	trunc := func(ms int64) (int64, string) {
		t := time.UnixMilli(ms).UTC()
		var tt time.Time
		switch interval {
		case "year", "1y":
			tt = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		case "month", "1M":
			tt = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		case "week", "1w":
			tt = t.Truncate(24 * time.Hour)
			for tt.Weekday() != time.Monday {
				tt = tt.AddDate(0, 0, -1)
			}
		case "hour", "1h":
			tt = t.Truncate(time.Hour)
		case "minute", "1m":
			tt = t.Truncate(time.Minute)
		default: // day
			tt = t.Truncate(24 * time.Hour)
		}
		return tt.UnixMilli(), tt.Format("2006-01-02T15:04:05.000Z")
	}

	type bucket struct {
		key   int64
		str   string
		docs  []matchedDoc
		count int
	}
	byKey := map[int64]*bucket{}
	for _, m := range docs {
		seen := map[int64]bool{}
		for _, v := range fieldValues(m, field) {
			s, ok := v.(string)
			if !ok {
				continue
			}
			ms, ok := index.ParseDate(s)
			if !ok {
				continue
			}
			k, ks := trunc(ms)
			if seen[k] {
				continue
			}
			seen[k] = true
			b := byKey[k]
			if b == nil {
				b = &bucket{key: k, str: ks}
				byKey[k] = b
			}
			b.count++
			b.docs = append(b.docs, m)
		}
	}
	list := make([]*bucket, 0, len(byKey))
	for _, b := range byKey {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].key < list[j].key })
	buckets := make([]map[string]interface{}, 0, len(list))
	for _, b := range list {
		bm := map[string]interface{}{"key": b.key, "key_as_string": b.str, "doc_count": b.count}
		if err := withSubAggs(indices, b.docs, subAggs, bm); err != nil {
			return nil, err
		}
		buckets = append(buckets, bm)
	}
	return map[string]interface{}{"buckets": buckets}, nil
}

func aggGlobal(indices []*index.Index, subAggs map[string]json.RawMessage) (interface{}, error) {
	var all []matchedDoc
	for _, ix := range indices {
		ix.EachDoc(func(id string, _ map[string][]interface{}) bool {
			all = append(all, matchedDoc{ix: ix, id: id})
			return true
		})
	}
	out := map[string]interface{}{"doc_count": len(all)}
	if err := withSubAggs(indices, all, subAggs, out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- metric aggs ----------------------------------------------------

func aggMetric(docs []matchedDoc, kind string, body map[string]interface{}) (interface{}, error) {
	field := toQueryString(body["field"])
	var nums []float64
	distinct := map[string]bool{}
	for _, m := range docs {
		for _, v := range fieldValues(m, field) {
			if kind == "cardinality" || kind == "value_count" {
				distinct[index.ToString(v)] = true
			}
			if n, ok := index.ToFloat(v); ok {
				nums = append(nums, n)
			}
		}
	}
	switch kind {
	case "value_count":
		count := 0
		for _, m := range docs {
			count += len(fieldValues(m, field))
		}
		return map[string]interface{}{"value": count}, nil
	case "cardinality":
		return map[string]interface{}{"value": len(distinct)}, nil
	}

	if len(nums) == 0 {
		if kind == "stats" {
			return map[string]interface{}{"count": 0, "min": nil, "max": nil, "avg": nil, "sum": 0}, nil
		}
		return map[string]interface{}{"value": nil}, nil
	}
	minV, maxV, sum := nums[0], nums[0], 0.0
	for _, n := range nums {
		if n < minV {
			minV = n
		}
		if n > maxV {
			maxV = n
		}
		sum += n
	}
	switch kind {
	case "min":
		return map[string]interface{}{"value": minV}, nil
	case "max":
		return map[string]interface{}{"value": maxV}, nil
	case "sum":
		return map[string]interface{}{"value": sum}, nil
	case "avg":
		return map[string]interface{}{"value": sum / float64(len(nums))}, nil
	case "stats":
		return map[string]interface{}{
			"count": len(nums), "min": minV, "max": maxV,
			"avg": sum / float64(len(nums)), "sum": sum,
		}, nil
	}
	return nil, fmt.Errorf("unsupported metric %q", kind)
}
