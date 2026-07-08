package server

import (
	"fmt"
	"net/http"
	"strings"
)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":         "stretchy-1",
		"cluster_name": "stretchy",
		"cluster_uuid": "stretchy-single-node",
		"version": map[string]interface{}{
			"number":                              esVersion,
			"build_flavor":                        "oss",
			"build_type":                          "tar",
			"build_hash":                          "stretchy-" + s.version,
			"build_date":                          "2026-01-01T00:00:00.000000Z",
			"build_snapshot":                      false,
			"lucene_version":                      "8.7.0",
			"minimum_wire_compatibility_version":  "6.8.0",
			"minimum_index_compatibility_version": "6.0.0-beta1",
		},
		"tagline": "You Know, for Search",
	})
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request, seg []string) {
	sub := ""
	if len(seg) > 1 {
		sub = seg[1]
	}
	switch sub {
	case "health":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"cluster_name":                     "stretchy",
			"status":                           "green",
			"timed_out":                        false,
			"number_of_nodes":                  1,
			"number_of_data_nodes":             1,
			"active_primary_shards":            len(s.store.Names()),
			"active_shards":                    len(s.store.Names()),
			"relocating_shards":                0,
			"initializing_shards":              0,
			"unassigned_shards":                0,
			"delayed_unassigned_shards":        0,
			"number_of_pending_tasks":          0,
			"number_of_in_flight_fetch":        0,
			"task_max_waiting_in_queue_millis": 0,
			"active_shards_percent_as_number":  100.0,
		})
	case "settings":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"persistent": map[string]interface{}{}, "transient": map[string]interface{}{},
		})
	case "stats":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"cluster_name": "stretchy", "status": "green",
			"indices": map[string]interface{}{"count": len(s.store.Names())},
			"nodes":   map[string]interface{}{"count": map[string]interface{}{"total": 1}},
		})
	default:
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"cluster_name": "stretchy"})
	}
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"_nodes":       map[string]interface{}{"total": 1, "successful": 1, "failed": 0},
		"cluster_name": "stretchy",
		"nodes": map[string]interface{}{
			"stretchy-node-1": map[string]interface{}{
				"name":    "stretchy-1",
				"version": esVersion,
				"roles":   []string{"data", "ingest", "master"},
				"plugins": []interface{}{},
				"modules": []interface{}{},
				"http": map[string]interface{}{
					"publish_address": s.http.Addr,
				},
			},
		},
	})
}

func (s *Server) handleCat(w http.ResponseWriter, r *http.Request, seg []string) {
	sub := ""
	if len(seg) > 1 {
		sub = seg[1]
	}
	asJSON := r.URL.Query().Get("format") == "json"

	switch sub {
	case "indices":
		type row struct {
			health, status, index, uuid string
			docs                        int
		}
		var rows []row
		for _, name := range s.store.Names() {
			ix, ok := s.store.Get(name)
			if !ok {
				continue
			}
			rows = append(rows, row{"green", "open", name, "stretchy-" + name, ix.DocCount()})
		}
		if asJSON {
			out := make([]map[string]interface{}, 0, len(rows))
			for _, rr := range rows {
				out = append(out, map[string]interface{}{
					"health": rr.health, "status": rr.status, "index": rr.index,
					"uuid": rr.uuid, "pri": "1", "rep": "0",
					"docs.count": fmt.Sprintf("%d", rr.docs), "docs.deleted": "0",
					"store.size": "0b", "pri.store.size": "0b",
				})
			}
			s.writeJSON(w, http.StatusOK, out)
			return
		}
		var b strings.Builder
		if r.URL.Query().Has("v") {
			b.WriteString("health status index uuid pri rep docs.count docs.deleted store.size pri.store.size\n")
		}
		for _, rr := range rows {
			fmt.Fprintf(&b, "%s %s %s %s 1 0 %d 0 0b 0b\n", rr.health, rr.status, rr.index, rr.uuid, rr.docs)
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.Write([]byte(b.String()))

	case "health":
		if asJSON {
			s.writeJSON(w, http.StatusOK, []map[string]interface{}{{"cluster": "stretchy", "status": "green"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		fmt.Fprintln(w, "stretchy green")

	case "nodes":
		if asJSON {
			s.writeJSON(w, http.StatusOK, []map[string]interface{}{{"name": "stretchy-1", "node.role": "dim", "master": "*"}})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		fmt.Fprintln(w, "stretchy-1 dim *")

	default:
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusOK)
	}
}
