package search

import (
	"encoding/json"
	"testing"

	"github.com/ostap-mykhaylyak/stretchy/internal/index"
	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
)

func testIndex(t *testing.T) *index.Index {
	t.Helper()
	log, err := logx.New("", "error")
	if err != nil {
		t.Fatal(err)
	}
	store, err := index.OpenStore(t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	ix, err := store.Create("products", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	docs := map[string]string{
		"1": `{"title":"Caffè Arabica 250g","price":12.5,"stock":10,"category":"caffe","post_date":"2026-01-10 09:00:00"}`,
		"2": `{"title":"Caffè Robusta 250g","price":9.9,"stock":0,"category":"caffe","post_date":"2026-02-01 09:00:00"}`,
		"3": `{"title":"Tè verde biologico","price":7.5,"stock":25,"category":"te","post_date":"2026-03-05 09:00:00"}`,
		"4": `{"title":"Macchina per caffè espresso","price":89.0,"stock":3,"category":"macchine","post_date":"2026-01-20 09:00:00"}`,
	}
	for id, src := range docs {
		if _, _, err := ix.Put(id, json.RawMessage(src)); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	return ix
}

func run(t *testing.T, ix *index.Index, body string) *Result {
	t.Helper()
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Exec([]*index.Index{ix}, req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ids(res *Result) map[string]bool {
	out := map[string]bool{}
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

func TestMatchWithAccentFolding(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"match":{"title":"caffe"}}}`)
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
}

func TestMatchOperatorAnd(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"match":{"title":{"query":"caffè espresso","operator":"and"}}}}`)
	if res.Total != 1 || !ids(res)["4"] {
		t.Fatalf("expected only doc 4, got %v", ids(res))
	}
}

func TestFuzzyMatch(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"match":{"title":{"query":"arabika","fuzziness":"AUTO"}}}}`)
	if !ids(res)["1"] {
		t.Fatalf("fuzzy match should find doc 1, got %v", ids(res))
	}
}

func TestBoolWithRangeFilter(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"bool":{
		"must":[{"match":{"title":"caffe"}}],
		"filter":[{"range":{"price":{"lte":15}}},{"range":{"stock":{"gt":0}}}]
	}}}`)
	if res.Total != 1 || !ids(res)["1"] {
		t.Fatalf("expected only doc 1, got %v", ids(res))
	}
}

func TestTermOnKeywordSubfield(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"term":{"category.keyword":"caffe"}}}`)
	if res.Total != 2 {
		t.Fatalf("total = %d, want 2", res.Total)
	}
}

func TestSortByPriceDesc(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"match_all":{}},"sort":[{"price":{"order":"desc"}}]}`)
	if len(res.Hits) != 4 || res.Hits[0].ID != "4" || res.Hits[3].ID != "3" {
		t.Fatalf("unexpected order: %v", res.Hits)
	}
	if res.Hits[0].Sort[0] != 89.0 {
		t.Fatalf("sort value = %v", res.Hits[0].Sort)
	}
}

func TestPagination(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"match_all":{}},"sort":[{"price":"asc"}],"from":1,"size":2}`)
	if res.Total != 4 || len(res.Hits) != 2 {
		t.Fatalf("total=%d hits=%d", res.Total, len(res.Hits))
	}
	if res.Hits[0].ID != "2" || res.Hits[1].ID != "1" {
		t.Fatalf("unexpected page: %s %s", res.Hits[0].ID, res.Hits[1].ID)
	}
}

func TestTermsAggregation(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"size":0,"aggs":{"cats":{"terms":{"field":"category.keyword"}}}}`)
	agg, ok := res.Aggs["cats"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing cats agg: %v", res.Aggs)
	}
	buckets := agg["buckets"].([]map[string]interface{})
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}
	if buckets[0]["key"] != "caffe" || buckets[0]["doc_count"] != 2 {
		t.Fatalf("top bucket = %v", buckets[0])
	}
}

func TestPostFilterKeepsAggs(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{
		"query":{"match_all":{}},
		"post_filter":{"term":{"category.keyword":"te"}},
		"aggs":{"cats":{"terms":{"field":"category.keyword"}}}
	}`)
	if res.Total != 1 || !ids(res)["3"] {
		t.Fatalf("post_filter failed: %v", ids(res))
	}
	buckets := res.Aggs["cats"].(map[string]interface{})["buckets"].([]map[string]interface{})
	if len(buckets) != 3 {
		t.Fatalf("aggs must ignore post_filter, got %d buckets", len(buckets))
	}
}

func TestHighlight(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{
		"query":{"match":{"title":"espresso"}},
		"highlight":{"fields":{"title":{}}}
	}`)
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d", len(res.Hits))
	}
	hl := res.Hits[0].Highlight["title"]
	if len(hl) != 1 || hl[0] != "Macchina per caffè <em>espresso</em>" {
		t.Fatalf("highlight = %v", hl)
	}
}

func TestRangeOnDate(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"range":{"post_date":{"gte":"2026-02-01 00:00:00"}}}}`)
	if res.Total != 2 || !ids(res)["2"] || !ids(res)["3"] {
		t.Fatalf("date range got %v", ids(res))
	}
}

func TestMultiMatchBestFields(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"multi_match":{"query":"caffe","fields":["title^3","category"]}}}`)
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
}

func TestFunctionScoreWeight(t *testing.T) {
	ix := testIndex(t)
	res := run(t, ix, `{"query":{"function_score":{
		"query":{"match":{"title":"caffe"}},
		"functions":[{"filter":{"term":{"category.keyword":"macchine"}},"weight":10}]
	}}}`)
	if len(res.Hits) == 0 || res.Hits[0].ID != "4" {
		t.Fatalf("weighted doc should rank first, got %v", res.Hits)
	}
}
