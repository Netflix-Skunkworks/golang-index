package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type server struct {
	port int
	idx  *index
}

func newServer(port int, index *index) *server {
	return &server{port: port, idx: index}
}

type JSONOut struct {
	Path      string `json:"Path"`
	Version   string `json:"Version"`
	Timestamp string `json:"Timestamp"`
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
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

	var lines []string
	for repoName, tags := range s.idx.repoTags {
		for _, tag := range tags {
			if tag.commitDate.Before(since) {
				continue
			}

			out, err := json.Marshal(&JSONOut{
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

func (s *server) listenAndServe() error {
	http.HandleFunc("/", s.handleIndex)
	fmt.Printf("Server listening on :%d\n", s.port)
	return http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil)
}
