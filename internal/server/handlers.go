package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ostap-mykhaylyak/stretchy/internal/analysis"
	"github.com/ostap-mykhaylyak/stretchy/internal/index"
)

// --- index lifecycle ------------------------------------------------

func (s *Server) handleIndexRoot(w http.ResponseWriter, r *http.Request, expr string) {
	switch r.Method {
	case http.MethodHead:
		if len(s.store.Resolve(expr)) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		indices := s.store.Resolve(expr)
		if len(indices) == 0 {
			s.indexNotFound(w, r, expr)
			return
		}
		out := map[string]interface{}{}
		for _, ix := range indices {
			out[ix.Name] = map[string]interface{}{
				"aliases":  map[string]interface{}{},
				"mappings": ix.Mapping.MarshalBody(),
				"settings": s.indexSettings(ix),
			}
		}
		s.writeJSON(w, http.StatusOK, out)

	case http.MethodPut:
		body, err := readBody(r)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
			return
		}
		var req struct {
			Settings json.RawMessage `json:"settings"`
			Mappings json.RawMessage `json:"mappings"`
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
				return
			}
		}
		// legacy typed mappings: {"mappings": {"post": {"properties": ...}}}
		req.Mappings = unwrapTypedMapping(req.Mappings)
		if _, err := s.store.Create(expr, req.Settings, req.Mappings); err != nil {
			if err == index.ErrIndexExists {
				s.errorJSON(w, r, http.StatusBadRequest, "resource_already_exists_exception",
					fmt.Sprintf("index [%s] already exists", expr))
				return
			}
			s.errorJSON(w, r, http.StatusBadRequest, "invalid_index_name_exception", err.Error())
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"acknowledged": true, "shards_acknowledged": true, "index": expr,
		})

	case http.MethodDelete:
		indices := s.store.Resolve(expr)
		if len(indices) == 0 {
			s.indexNotFound(w, r, expr)
			return
		}
		for _, ix := range indices {
			if err := s.store.Delete(ix.Name); err != nil {
				s.errorJSON(w, r, http.StatusInternalServerError, "internal_error", err.Error())
				return
			}
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})

	default:
		s.errorJSON(w, r, http.StatusMethodNotAllowed, "illegal_argument_exception", "unsupported method")
	}
}

// unwrapTypedMapping strips a legacy single-type layer when present.
func unwrapTypedMapping(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, ok := m["properties"]; ok {
		return raw
	}
	if len(m) == 1 {
		for _, inner := range m {
			var im map[string]json.RawMessage
			if err := json.Unmarshal(inner, &im); err == nil {
				if _, ok := im["properties"]; ok {
					return inner
				}
			}
		}
	}
	return raw
}

func (s *Server) indexSettings(ix *index.Index) map[string]interface{} {
	var user map[string]interface{}
	json.Unmarshal(ix.Settings(), &user)
	if user == nil {
		user = map[string]interface{}{}
	}
	if _, ok := user["index"]; !ok {
		user["index"] = map[string]interface{}{}
	}
	if im, ok := user["index"].(map[string]interface{}); ok {
		if _, ok := im["number_of_shards"]; !ok {
			im["number_of_shards"] = "1"
		}
		if _, ok := im["number_of_replicas"]; !ok {
			im["number_of_replicas"] = "0"
		}
		im["uuid"] = "stretchy-" + ix.Name
		im["provided_name"] = ix.Name
	}
	return user
}

func (s *Server) handleMapping(w http.ResponseWriter, r *http.Request, expr string) {
	switch r.Method {
	case http.MethodGet:
		indices := s.store.Resolve(expr)
		if len(indices) == 0 {
			s.indexNotFound(w, r, expr)
			return
		}
		out := map[string]interface{}{}
		for _, ix := range indices {
			out[ix.Name] = map[string]interface{}{"mappings": ix.Mapping.MarshalBody()}
		}
		s.writeJSON(w, http.StatusOK, out)

	case http.MethodPut, http.MethodPost:
		ix, err := s.store.GetOrCreate(expr)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "invalid_index_name_exception", err.Error())
			return
		}
		body, err := readBody(r)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
			return
		}
		if err := ix.Mapping.UnmarshalBody(unwrapTypedMapping(body)); err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "mapper_parsing_exception", err.Error())
			return
		}
		ix.SaveMeta()
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})

	default:
		s.errorJSON(w, r, http.StatusMethodNotAllowed, "illegal_argument_exception", "unsupported method")
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, expr string) {
	switch r.Method {
	case http.MethodGet:
		indices := s.store.Resolve(expr)
		if len(indices) == 0 {
			s.indexNotFound(w, r, expr)
			return
		}
		out := map[string]interface{}{}
		for _, ix := range indices {
			out[ix.Name] = map[string]interface{}{"settings": s.indexSettings(ix)}
		}
		s.writeJSON(w, http.StatusOK, out)

	case http.MethodPut:
		indices := s.store.Resolve(expr)
		if len(indices) == 0 {
			s.indexNotFound(w, r, expr)
			return
		}
		body, _ := readBody(r)
		for _, ix := range indices {
			merged := mergeSettings(ix.Settings(), body)
			ix.SetSettings(merged)
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})

	default:
		s.errorJSON(w, r, http.StatusMethodNotAllowed, "illegal_argument_exception", "unsupported method")
	}
}

func mergeSettings(base, overlay json.RawMessage) json.RawMessage {
	var a, b map[string]interface{}
	json.Unmarshal(base, &a)
	json.Unmarshal(overlay, &b)
	if a == nil {
		a = map[string]interface{}{}
	}
	for k, v := range b {
		a[k] = v
	}
	out, err := json.Marshal(a)
	if err != nil {
		return base
	}
	return out
}

// --- documents ------------------------------------------------------

// handleDoc serves /{index}/_doc[/{id}] and /{index}/_create/{id}.
func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request, indexName string, rest []string) {
	var id string
	if len(rest) > 1 {
		id = rest[1]
	}
	create := rest[0] == "_create"

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		ix, ok := s.store.Get(indexName)
		if !ok {
			s.indexNotFound(w, r, indexName)
			return
		}
		doc, found := ix.Get(id)
		if !found {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"_index": indexName, "_id": id, "found": false,
			})
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"_index": indexName, "_id": id, "_version": doc.Version,
			"_seq_no": doc.SeqNo, "_primary_term": 1,
			"found": true, "_source": doc.Source,
		})

	case http.MethodPut, http.MethodPost:
		body, err := readBody(r)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
			return
		}
		ix, err := s.store.GetOrCreate(indexName)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "invalid_index_name_exception", err.Error())
			return
		}
		if id == "" {
			id = generateID()
		}
		if create {
			if _, exists := ix.Get(id); exists {
				s.errorJSON(w, r, http.StatusConflict, "version_conflict_engine_exception",
					fmt.Sprintf("[%s]: version conflict, document already exists", id))
				return
			}
		}
		result, version, err := ix.Put(id, body)
		if err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "mapper_parsing_exception", err.Error())
			return
		}
		status := http.StatusOK
		if result == "created" {
			status = http.StatusCreated
		}
		s.writeJSON(w, status, docResponse(indexName, id, result, version))

	case http.MethodDelete:
		ix, ok := s.store.Get(indexName)
		if !ok {
			s.indexNotFound(w, r, indexName)
			return
		}
		if !ix.Delete(id) {
			s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"_index": indexName, "_id": id, "result": "not_found",
				"_shards": shardsOK(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, docResponse(indexName, id, "deleted", 1))

	default:
		s.errorJSON(w, r, http.StatusMethodNotAllowed, "illegal_argument_exception", "unsupported method")
	}
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request, indexName string, rest []string) {
	if len(rest) < 2 {
		s.errorJSON(w, r, http.StatusBadRequest, "illegal_argument_exception", "missing document id")
		return
	}
	ix, ok := s.store.Get(indexName)
	if !ok {
		s.indexNotFound(w, r, indexName)
		return
	}
	doc, found := ix.Get(rest[1])
	if !found {
		s.errorJSON(w, r, http.StatusNotFound, "resource_not_found_exception",
			fmt.Sprintf("document [%s] not found", rest[1]))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(doc.Source)
	}
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, indexName string, rest []string) {
	if len(rest) < 2 {
		s.errorJSON(w, r, http.StatusBadRequest, "illegal_argument_exception", "missing document id")
		return
	}
	id := rest[1]
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	var req struct {
		Doc         json.RawMessage `json:"doc"`
		Upsert      json.RawMessage `json:"upsert"`
		DocAsUpsert bool            `json:"doc_as_upsert"`
		Script      json.RawMessage `json:"script"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	if req.Doc == nil && req.Script != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "illegal_argument_exception",
			"scripted updates are not supported by stretchy")
		return
	}
	ix, err := s.store.GetOrCreate(indexName)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "invalid_index_name_exception", err.Error())
		return
	}
	upsert := req.Upsert
	if upsert == nil && req.DocAsUpsert {
		upsert = req.Doc
	}
	result, version, err := ix.Update(id, req.Doc, upsert)
	if err != nil {
		if err == index.ErrDocMissing {
			s.errorJSON(w, r, http.StatusNotFound, "document_missing_exception",
				fmt.Sprintf("[%s]: document missing", id))
			return
		}
		s.errorJSON(w, r, http.StatusBadRequest, "mapper_parsing_exception", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, docResponse(indexName, id, result, version))
}

func (s *Server) handleMget(w http.ResponseWriter, r *http.Request, indexName string) {
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	var req struct {
		Docs []struct {
			Index string `json:"_index"`
			ID    string `json:"_id"`
		} `json:"docs"`
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}
	for _, id := range req.IDs {
		req.Docs = append(req.Docs, struct {
			Index string `json:"_index"`
			ID    string `json:"_id"`
		}{Index: indexName, ID: id})
	}
	docs := make([]interface{}, 0, len(req.Docs))
	for _, d := range req.Docs {
		name := d.Index
		if name == "" {
			name = indexName
		}
		entry := map[string]interface{}{"_index": name, "_id": d.ID, "found": false}
		if ix, ok := s.store.Get(name); ok {
			if doc, found := ix.Get(d.ID); found {
				entry["found"] = true
				entry["_version"] = doc.Version
				entry["_source"] = doc.Source
			}
		}
		docs = append(docs, entry)
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"docs": docs})
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var req struct {
		Text interface{} `json:"text"`
	}
	json.Unmarshal(body, &req)
	var texts []string
	switch t := req.Text.(type) {
	case string:
		texts = []string{t}
	case []interface{}:
		for _, v := range t {
			if s, ok := v.(string); ok {
				texts = append(texts, s)
			}
		}
	}
	tokens := []map[string]interface{}{}
	pos := 0
	for _, text := range texts {
		for _, tok := range analysis.Analyze(text) {
			tokens = append(tokens, map[string]interface{}{
				"token": tok.Term, "type": "<ALPHANUM>",
				"position": pos, "start_offset": 0, "end_offset": len(tok.Term),
			})
			pos++
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"tokens": tokens})
}

// handleStub acknowledges template/pipeline management APIs that
// WordPress plugins call but stretchy doesn't need.
func (s *Server) handleStub(w http.ResponseWriter, r *http.Request, seg []string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{})
	default:
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
	}
}

func docResponse(indexName, id, result string, version int64) map[string]interface{} {
	return map[string]interface{}{
		"_index": indexName, "_type": "_doc", "_id": id,
		"_version": version, "result": result,
		"_shards": shardsOK(), "_seq_no": version, "_primary_term": 1,
	}
}

var idCounter uint64

func generateID() string {
	n := atomic.AddUint64(&idCounter, 1)
	return fmt.Sprintf("st-%d-%d", time.Now().UnixNano(), n)
}
