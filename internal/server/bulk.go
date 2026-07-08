package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ostap-mykhaylyak/stretchy/internal/index"
)

type bulkAction struct {
	Index  *bulkMeta `json:"index"`
	Create *bulkMeta `json:"create"`
	Update *bulkMeta `json:"update"`
	Delete *bulkMeta `json:"delete"`
}

type bulkMeta struct {
	Index string `json:"_index"`
	ID    string `json:"_id"`
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request, defaultIndex string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		s.errorJSON(w, r, http.StatusMethodNotAllowed, "illegal_argument_exception", "bulk requires POST")
		return
	}
	body, err := readBody(r)
	if err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}

	start := time.Now()
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	var items []map[string]interface{}
	hadErrors := false

	appendItem := func(op, indexName, id string, status int, result string, errType, errReason string) {
		entry := map[string]interface{}{
			"_index": indexName, "_type": "_doc", "_id": id, "status": status,
		}
		if errType != "" {
			hadErrors = true
			entry["error"] = map[string]interface{}{"type": errType, "reason": errReason}
		} else {
			entry["result"] = result
			entry["_shards"] = shardsOK()
			entry["_version"] = 1
			entry["_seq_no"] = 0
			entry["_primary_term"] = 1
		}
		items = append(items, map[string]interface{}{op: entry})
	}

	readLine := func() ([]byte, bool) {
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) > 0 {
				return line, true
			}
		}
		return nil, false
	}

	for {
		line, ok := readLine()
		if !ok {
			break
		}
		var action bulkAction
		if err := json.Unmarshal(line, &action); err != nil {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", "malformed action line: "+err.Error())
			return
		}

		var op string
		var meta *bulkMeta
		switch {
		case action.Index != nil:
			op, meta = "index", action.Index
		case action.Create != nil:
			op, meta = "create", action.Create
		case action.Update != nil:
			op, meta = "update", action.Update
		case action.Delete != nil:
			op, meta = "delete", action.Delete
		default:
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", "action line missing operation")
			return
		}

		indexName := meta.Index
		if indexName == "" {
			indexName = defaultIndex
		}
		id := meta.ID

		if op == "delete" {
			ix, ok := s.store.Get(indexName)
			if !ok || !ix.Delete(id) {
				appendItem(op, indexName, id, http.StatusNotFound, "not_found", "", "")
				continue
			}
			appendItem(op, indexName, id, http.StatusOK, "deleted", "", "")
			continue
		}

		src, ok := readLine()
		if !ok {
			s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", "missing source line for "+op)
			return
		}
		srcCopy := make([]byte, len(src))
		copy(srcCopy, src)

		ix, err := s.store.GetOrCreate(indexName)
		if err != nil {
			appendItem(op, indexName, id, http.StatusBadRequest, "", "invalid_index_name_exception", err.Error())
			continue
		}

		switch op {
		case "index", "create":
			if id == "" {
				id = generateID()
			}
			if op == "create" {
				if _, exists := ix.Get(id); exists {
					appendItem(op, indexName, id, http.StatusConflict, "",
						"version_conflict_engine_exception", "document already exists")
					continue
				}
			}
			result, _, err := ix.Put(id, srcCopy)
			if err != nil {
				appendItem(op, indexName, id, http.StatusBadRequest, "", "mapper_parsing_exception", err.Error())
				continue
			}
			status := http.StatusOK
			if result == "created" {
				status = http.StatusCreated
			}
			appendItem(op, indexName, id, status, result, "", "")

		case "update":
			var upd struct {
				Doc         json.RawMessage `json:"doc"`
				Upsert      json.RawMessage `json:"upsert"`
				DocAsUpsert bool            `json:"doc_as_upsert"`
			}
			if err := json.Unmarshal(srcCopy, &upd); err != nil {
				appendItem(op, indexName, id, http.StatusBadRequest, "", "parse_exception", err.Error())
				continue
			}
			upsert := upd.Upsert
			if upsert == nil && upd.DocAsUpsert {
				upsert = upd.Doc
			}
			result, _, err := ix.Update(id, upd.Doc, upsert)
			if err != nil {
				if err == index.ErrDocMissing {
					appendItem(op, indexName, id, http.StatusNotFound, "", "document_missing_exception", "document missing")
				} else {
					appendItem(op, indexName, id, http.StatusBadRequest, "", "mapper_parsing_exception", err.Error())
				}
				continue
			}
			appendItem(op, indexName, id, http.StatusOK, result, "", "")
		}
	}
	if err := scanner.Err(); err != nil {
		s.errorJSON(w, r, http.StatusBadRequest, "parse_exception", err.Error())
		return
	}

	if items == nil {
		items = []map[string]interface{}{}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"took":   time.Since(start).Milliseconds(),
		"errors": hadErrors,
		"items":  items,
	})
}
