package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ostap-mykhaylyak/stretchy/internal/search"
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, indexExpr string) {
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	req, err := search.ParseRequest(body)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}

	// URI-search conveniences
	q := r.URL.Query()
	if v := q.Get("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			req.Size = &n
		}
	}
	if v := q.Get("from"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			req.From = n
		}
	}
	if v := q.Get("q"); v != "" && len(req.Query) == 0 {
		raw, _ := json.Marshal(map[string]interface{}{
			"query_string": map[string]interface{}{"query": v},
		})
		req.Query = raw
	}
	if v := q.Get("sort"); v != "" && len(req.Sort) == 0 {
		raw, _ := json.Marshal([]string{v})
		req.Sort = raw
	}

	indices := s.store.Resolve(indexExpr)
	if len(indices) == 0 && indexExpr != "*" && indexExpr != "_all" {
		if q.Get("ignore_unavailable") != "true" {
			s.indexNotFound(w, r, indexExpr)
			return
		}
	}

	start := time.Now()
	res, err := search.Exec(indices, req)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "search_phase_execution_exception", err.Error())
		return
	}

	hits := make([]map[string]interface{}, 0, len(res.Hits))
	for _, h := range res.Hits {
		hit := map[string]interface{}{
			"_index": h.Index, "_type": "_doc", "_id": h.ID,
		}
		if h.Sort != nil {
			hit["sort"] = h.Sort
			hit["_score"] = nil
		} else {
			hit["_score"] = h.Score
		}
		if h.Source != nil {
			hit["_source"] = h.Source
		}
		if len(h.Highlight) > 0 {
			hit["highlight"] = h.Highlight
		}
		hits = append(hits, hit)
	}

	envelope := map[string]interface{}{
		"took":      time.Since(start).Milliseconds(),
		"timed_out": false,
		"_shards":   shardsOK(),
		"hits": map[string]interface{}{
			"total":     map[string]interface{}{"value": res.Total, "relation": "eq"},
			"max_score": res.MaxScore,
			"hits":      hits,
		},
	}
	if res.Aggs != nil {
		envelope["aggregations"] = res.Aggs
	}
	s.writeJSON(w, http.StatusOK, envelope)
}

func (s *Server) handleCount(w http.ResponseWriter, r *http.Request, indexExpr string) {
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	var req struct {
		Query json.RawMessage `json:"query"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
			return
		}
	}
	indices := s.store.Resolve(indexExpr)
	total := 0
	for _, ix := range indices {
		matches, err := search.Matches(ix, req.Query)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "search_phase_execution_exception", err.Error())
			return
		}
		total += len(matches)
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": total, "_shards": shardsOK(),
	})
}

func (s *Server) handleDeleteByQuery(w http.ResponseWriter, r *http.Request, indexExpr string) {
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	var req struct {
		Query json.RawMessage `json:"query"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
			return
		}
	}
	indices := s.store.Resolve(indexExpr)
	if len(indices) == 0 {
		s.indexNotFound(w, r, indexExpr)
		return
	}
	start := time.Now()
	deleted := 0
	for _, ix := range indices {
		matches, err := search.Matches(ix, req.Query)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "search_phase_execution_exception", err.Error())
			return
		}
		for id := range matches {
			if ix.Delete(id) {
				deleted++
			}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"took": time.Since(start).Milliseconds(), "timed_out": false,
		"total": deleted, "deleted": deleted, "batches": 1,
		"version_conflicts": 0, "noops": 0, "failures": []interface{}{},
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, indexExpr string) {
	indices := s.store.Resolve(indexExpr)
	perIndex := map[string]interface{}{}
	totalDocs := 0
	for _, ix := range indices {
		n := ix.DocCount()
		totalDocs += n
		stats := map[string]interface{}{
			"docs": map[string]interface{}{"count": n, "deleted": 0},
		}
		perIndex[ix.Name] = map[string]interface{}{
			"primaries": stats, "total": stats,
		}
	}
	all := map[string]interface{}{
		"docs": map[string]interface{}{"count": totalDocs, "deleted": 0},
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"_shards": shardsOK(),
		"_all":    map[string]interface{}{"primaries": all, "total": all},
		"indices": perIndex,
	})
}

func fmtBytes(n int) string {
	return fmt.Sprintf("%db", n)
}
