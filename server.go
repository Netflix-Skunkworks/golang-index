package main

import (
	"encoding/json"
	"fmt"
	"log"
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
	var lines []string

	s.idx.mu.RLock()
	defer s.idx.mu.RUnlock()

	var since time.Time
	var err error
	if sinceParam := r.URL.Query().Get("since"); sinceParam != "" {
		since, err = time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(fmt.Sprintf("error converting 'since' param %s: %v", sinceParam, err)))
			return
		}
	}

	for repoName, tags := range s.idx.repoTags {
		for _, tag := range tags {
			timestamp := tag.commitDate
			if !since.IsZero() && timestamp.Before(since) {
				continue
			}

			jo := JSONOut{Path: fmt.Sprintf("github.netflix.net/%s", repoName), Version: tag.tag, Timestamp: timestamp.Format(time.RFC3339)}
			out, err := json.Marshal(&jo)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(fmt.Sprintf("error marshalling response for tag %s: %v", tag.tag, err)))
				return
			}

			lines = append(lines, string(out))
		}
	}
	if _, err := w.Write([]byte(strings.Join(lines, "\n"))); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("error writing response: %v", err)))
		return
	}
}

func (s *server) listenAndServe() {
	http.HandleFunc("/", s.handleIndex)
	fmt.Printf("Server listening on :%d\n", s.port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil))
}
