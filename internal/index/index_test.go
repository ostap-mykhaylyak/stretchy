package index

import (
	"encoding/json"
	"testing"

	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
)

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	log, _ := logx.New("", "error")

	store, err := OpenStore(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := store.Create("posts", nil, json.RawMessage(`{"properties":{"title":{"type":"text"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Put("1", json.RawMessage(`{"title":"hello world"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Put("2", json.RawMessage(`{"title":"goodbye"}`)); err != nil {
		t.Fatal(err)
	}
	if !ix.Delete("2") {
		t.Fatal("delete failed")
	}
	store.Close()

	store2, err := OpenStore(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	ix2, ok := store2.Get("posts")
	if !ok {
		t.Fatal("index not reloaded")
	}
	if n := ix2.DocCount(); n != 1 {
		t.Fatalf("doc count after reopen = %d, want 1", n)
	}
	doc, found := ix2.Get("1")
	if !found {
		t.Fatal("doc 1 missing after reopen")
	}
	if string(doc.Source) != `{"title":"hello world"}` {
		t.Fatalf("source = %s", doc.Source)
	}
	if ix2.Mapping.FieldType("title") != TypeText {
		t.Fatalf("mapping lost: %q", ix2.Mapping.FieldType("title"))
	}
	// inverted index rebuilt
	if hits := ix2.PostingDocs("title", "hello"); len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("posting after reopen = %v", hits)
	}
}

func TestUpdateMerge(t *testing.T) {
	log, _ := logx.New("", "error")
	store, err := OpenStore(t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ix, _ := store.Create("posts", nil, nil)
	ix.Put("1", json.RawMessage(`{"title":"ciao","meta":{"views":1}}`))
	if _, _, err := ix.Update("1", json.RawMessage(`{"meta":{"views":2}}`), nil); err != nil {
		t.Fatal(err)
	}
	doc, _ := ix.Get("1")
	var src map[string]interface{}
	json.Unmarshal(doc.Source, &src)
	if src["title"] != "ciao" {
		t.Fatalf("title lost on partial update: %v", src)
	}
	if src["meta"].(map[string]interface{})["views"].(float64) != 2 {
		t.Fatalf("views not updated: %v", src)
	}
}

func TestDynamicMapping(t *testing.T) {
	log, _ := logx.New("", "error")
	store, err := OpenStore(t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ix, _ := store.Create("posts", nil, nil)
	ix.Put("1", json.RawMessage(`{"name":"test","qty":5,"price":1.5,"ok":true,"when":"2026-07-01 10:00:00"}`))

	cases := map[string]string{
		"name": TypeText, "name.keyword": TypeKeyword,
		"qty": TypeLong, "price": TypeDouble,
		"ok": TypeBool, "when": TypeDate,
	}
	for field, want := range cases {
		if got := ix.Mapping.FieldType(field); got != want {
			t.Errorf("FieldType(%s) = %q, want %q", field, got, want)
		}
	}
}
