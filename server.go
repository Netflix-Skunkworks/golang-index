package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"golang.org/x/exp/slog"
)

const defaultNumberOfOutputs = int64(2000)

// Exists to allow tests to mock the db.
type idb interface {
	FetchRepoTags(ctx context.Context, since time.Time, limit int64) ([]*db.RepoTag, error)
}

type server struct {
	port           int
	idb            idb
	githubHostName string
}

func newServer(port int, idb idb, githubHostName string) *server {
	return &server{port: port, idb: idb, githubHostName: githubHostName}
}

type module struct {
	Path      string `json:"Path"`
	Version   string `json:"Version"`
	Timestamp string `json:"Timestamp"`
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	var err error
	if sinceParam := r.URL.Query().Get("since"); sinceParam != "" {
		since, err = time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			http.Error(w, fmt.Sprintf("error converting 'since' param %s: %v", sinceParam, err), http.StatusBadRequest)
			return
		}
	}

	limit := defaultNumberOfOutputs
	if limitParam := r.URL.Query().Get("limit"); limitParam != "" {
		if limit, err = strconv.ParseInt(limitParam, 10, 64); err != nil {
			http.Error(w, fmt.Sprintf("error converting 'limit' param %s: %v", limitParam, err), http.StatusBadRequest)
			return
		}
	}

	repoTags, err := s.idb.FetchRepoTags(r.Context(), since, limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("error fetching repo tags: %v", err), http.StatusInternalServerError)
		return
	}

	var lines []string
	for _, rt := range repoTags {
		out, err := json.Marshal(&module{
			Path:      rt.ModulePath,
			Version:   rt.TagName,
			Timestamp: rt.Created.Format(time.RFC3339),
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("error marshalling response for %v: %v", rt, err), http.StatusInternalServerError)
			return
		}

		lines = append(lines, string(out))
	}

	if _, err := fmt.Fprint(w, strings.Join(lines, "\n")); err != nil {
		http.Error(w, fmt.Sprintf("error writing response: %v", err), http.StatusInternalServerError)
		return
	}
}

func (s *server) listenAndServe() error {
	http.HandleFunc("/", s.handleIndex)
	slog.Info(fmt.Sprintf("Server listening on :%d\n", s.port))
	return http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil)
}
