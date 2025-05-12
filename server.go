package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultNumberOfOutputs = 2000

type server struct {
	port int

	mu  sync.RWMutex
	idx *index
}

func newServer(port int, index *index) *server {
	return &server{port: port, idx: index}
}

type module struct {
	Path      string `json:"Path"`
	Version   string `json:"Version"`
	Timestamp string `json:"Timestamp"`
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.idx.mu.RLock()
	defer s.idx.mu.RUnlock()

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
		if limit, err = strconv.Atoi(limitParam); err != nil {
			http.Error(w, fmt.Sprintf("error converting 'limit' param %s: %v", limitParam, err), http.StatusBadRequest)
			return
		}
	}

	var count int
	var lines []string

repoTags:
	for repoName, tags := range s.idx.repoTags {
		for _, tag := range tags {
			if tag.commitDate.Before(since) {
				continue
			}

			count += 1
			if count > limit {
				break repoTags
			}

			out, err := json.Marshal(&module{
				Path:      fmt.Sprintf("github.netflix.net/%s", repoName),
				Version:   tag.tag,
				Timestamp: tag.commitDate.Format(time.RFC3339),
			})
			if err != nil {
				http.Error(w, fmt.Sprintf("error marshalling response for tag %s: %v", tag.tag, err), http.StatusInternalServerError)
				return
			}

			lines = append(lines, string(out))
		}
	}

	if _, err := fmt.Fprint(w, strings.Join(lines, "\n")); err != nil {
		http.Error(w, fmt.Sprintf("error writing response: %v", err), http.StatusInternalServerError)
		return
	}
}

func (s *server) updateIndex(newIndex *index) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.idx = newIndex
}

func (s *server) listenAndServe() error {
	http.HandleFunc("/", s.handleIndex)
	fmt.Printf("Server listening on :%d\n", s.port)
	return http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil)
}
