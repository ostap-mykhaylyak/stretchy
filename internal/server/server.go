// Package server exposes the Elasticsearch-compatible REST API.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ostap-mykhaylyak/stretchy/internal/config"
	"github.com/ostap-mykhaylyak/stretchy/internal/index"
	"github.com/ostap-mykhaylyak/stretchy/internal/logx"
)

// esVersion is what stretchy reports to clients. 7.10.2 keeps every
// WordPress integration (ElasticPress & co.) on the widely supported
// 7.x request/response formats.
const esVersion = "7.10.2"

const maxBodySize = 256 << 20 // 256 MiB, bulk reindexing can be large

type Server struct {
	cfg     *config.Config
	store   *index.Store
	log     *logx.Logger
	version string
	http    *http.Server
}

func New(cfg *config.Config, store *index.Store, log *logx.Logger, version string) *Server {
	s := &Server{cfg: cfg, store: store, log: log, version: version}
	s.http = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe() error {
	s.log.Info("listening on http://%s", s.http.Addr)
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.http.Shutdown(ctx)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Elastic-Product", "Elasticsearch")

	if s.cfg.Auth.Username != "" || s.cfg.Auth.Password != "" {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Auth.Username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.Auth.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="stretchy"`)
			s.errorJSON(w, r, http.StatusUnauthorized, "security_exception", "missing or invalid credentials")
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
			s.errorJSON(w, r, http.StatusInternalServerError, "internal_error", fmt.Sprintf("%v", rec))
		}
	}()

	start := time.Now()
	s.route(w, r)
	s.log.Debug("%s %s (%s)", r.Method, r.URL.RequestURI(), time.Since(start))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	seg := []string{}
	if path != "" {
		seg = strings.Split(path, "/")
	}

	if len(seg) == 0 {
		s.handleRoot(w, r)
		return
	}

	// global endpoints
	switch seg[0] {
	case "_cluster":
		s.handleCluster(w, r, seg)
		return
	case "_nodes":
		s.handleNodes(w, r)
		return
	case "_cat":
		s.handleCat(w, r, seg)
		return
	case "_bulk":
		s.handleBulk(w, r, "")
		return
	case "_search":
		s.handleSearch(w, r, "*")
		return
	case "_count":
		s.handleCount(w, r, "*")
		return
	case "_mget":
		s.handleMget(w, r, "")
		return
	case "_refresh", "_flush", "_forcemerge":
		s.ackShards(w)
		return
	case "_stats":
		s.handleStats(w, r, "*")
		return
	case "_aliases":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
		return
	case "_alias":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{})
		return
	case "_template", "_index_template", "_ingest", "_scripts", "_ilm", "_xpack", "_license", "_security", "_ml", "_sql":
		s.handleStub(w, r, seg)
		return
	case "_analyze":
		s.handleAnalyze(w, r)
		return
	case "favicon.ico":
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// index-scoped endpoints
	indexExpr := seg[0]
	rest := seg[1:]

	if len(rest) == 0 {
		s.handleIndexRoot(w, r, indexExpr)
		return
	}

	switch rest[0] {
	case "_doc", "_create":
		s.handleDoc(w, r, indexExpr, rest)
	case "_source":
		s.handleSource(w, r, indexExpr, rest)
	case "_update":
		s.handleUpdate(w, r, indexExpr, rest)
	case "_search":
		s.handleSearch(w, r, indexExpr)
	case "_count":
		s.handleCount(w, r, indexExpr)
	case "_bulk":
		s.handleBulk(w, r, indexExpr)
	case "_mapping", "_mappings":
		s.handleMapping(w, r, indexExpr)
	case "_settings":
		s.handleSettings(w, r, indexExpr)
	case "_refresh", "_flush", "_forcemerge", "_cache":
		s.ackShards(w)
	case "_stats":
		s.handleStats(w, r, indexExpr)
	case "_analyze":
		s.handleAnalyze(w, r)
	case "_delete_by_query":
		s.handleDeleteByQuery(w, r, indexExpr)
	case "_mget":
		s.handleMget(w, r, indexExpr)
	case "_open", "_close":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"acknowledged": true})
	case "_alias", "_aliases":
		s.writeJSON(w, http.StatusOK, map[string]interface{}{indexExpr: map[string]interface{}{"aliases": map[string]interface{}{}}})
	default:
		if strings.HasPrefix(rest[0], "_") {
			s.errorJSON(w, r, http.StatusBadRequest, "illegal_argument_exception",
				fmt.Sprintf("request [%s] contains unrecognized endpoint [%s]", r.URL.Path, rest[0]))
			return
		}
		// legacy typed API: /{index}/{type}/{id}, /{index}/{type}/_search
		if len(rest) >= 2 {
			switch rest[1] {
			case "_search":
				s.handleSearch(w, r, indexExpr)
				return
			case "_count":
				s.handleCount(w, r, indexExpr)
				return
			case "_mapping":
				s.handleMapping(w, r, indexExpr)
				return
			}
			if len(rest) >= 3 && rest[2] == "_update" {
				s.handleUpdate(w, r, indexExpr, []string{"_update", rest[1]})
				return
			}
			s.handleDoc(w, r, indexExpr, append([]string{"_doc"}, rest[1:]...))
			return
		}
		s.errorJSON(w, r, http.StatusBadRequest, "illegal_argument_exception",
			fmt.Sprintf("unsupported path [%s]", r.URL.Path))
	}
}

// --- helpers --------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.log.Error("marshal response: %v", err)
		status = http.StatusInternalServerError
		raw = []byte(`{"error":{"type":"internal_error","reason":"response marshalling failed"},"status":500}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	w.Write(raw)
}

func (s *Server) errorJSON(w http.ResponseWriter, r *http.Request, status int, errType, reason string) {
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	s.writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"root_cause": []map[string]interface{}{{"type": errType, "reason": reason}},
			"type":       errType,
			"reason":     reason,
		},
		"status": status,
	})
}

func (s *Server) indexNotFound(w http.ResponseWriter, r *http.Request, name string) {
	s.errorJSON(w, r, http.StatusNotFound, "index_not_found_exception",
		fmt.Sprintf("no such index [%s]", name))
}

func (s *Server) ackShards(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"_shards": map[string]interface{}{"total": 1, "successful": 1, "failed": 0},
	})
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func shardsOK() map[string]interface{} {
	return map[string]interface{}{"total": 1, "successful": 1, "skipped": 0, "failed": 0}
}
