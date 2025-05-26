package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestIndexHandler(t *testing.T) {
	fakedRepos := map[string][]*repoTag{
		"repo1": []*repoTag{
			{tag: "tag1", tagDate: time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)},
			{tag: "tag2", tagDate: time.Date(2025, 2, 3, 4, 5, 6, 7, time.UTC)},
			{tag: "tag3", tagDate: time.Date(2025, 3, 4, 5, 6, 7, 8, time.UTC)},
		},
	}

	for _, tc := range []struct {
		name           string
		sinceParam     string
		limitParam     string
		tags           map[string][]*repoTag
		wantStatusCode int
		wantResponse   string
	}{
		{
			name:           "empty response",
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "response with tags",
			tags:           fakedRepos,
			wantStatusCode: http.StatusOK,
			wantResponse: "" +
				`{"Path":"github.netflix.net/repo1","Version":"tag1","Timestamp":"2025-01-02T03:04:05Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag3","Timestamp":"2025-03-04T05:06:07Z"}`,
		},
		{
			name:           "with 'since' query param",
			sinceParam:     "2025-02-01T00:00:00Z",
			tags:           fakedRepos,
			wantStatusCode: http.StatusOK,
			wantResponse: "" +
				`{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag3","Timestamp":"2025-03-04T05:06:07Z"}`,
		},
		{
			name:           "with invalid 'since' query param",
			sinceParam:     "invalid",
			tags:           fakedRepos,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "with 'limit' query param",
			limitParam:     "1",
			tags:           fakedRepos,
			wantStatusCode: http.StatusOK,
			wantResponse:   `{"Path":"github.netflix.net/repo1","Version":"tag1","Timestamp":"2025-01-02T03:04:05Z"}`,
		},
		{
			name:           "with invalid 'limit' query param",
			limitParam:     "invalid",
			tags:           fakedRepos,
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:           "with both 'limit' and 'since' query params",
			sinceParam:     "2025-02-01T00:00:00Z",
			limitParam:     "1",
			tags:           fakedRepos,
			wantStatusCode: http.StatusOK,
			wantResponse:   `{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(0, &index{repoTags: tc.tags})

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			query := request.URL.Query()
			if tc.sinceParam != "" {
				query.Add("since", tc.sinceParam)
			}
			if tc.limitParam != "" {
				query.Add("limit", tc.limitParam)
			}
			request.URL.RawQuery = query.Encode()

			recorder := httptest.NewRecorder()

			s.handleIndex(recorder, request)

			if tc.wantStatusCode != recorder.Code {
				t.Errorf("wanted status code %d, got %d", tc.wantStatusCode, recorder.Code)
			}
			if tc.wantStatusCode == http.StatusOK {
				body, err := io.ReadAll(recorder.Body)
				if err != nil {
					t.Errorf("unexpected error while reading recorder body: %v", err)
				}
				if tc.wantResponse != string(body) {
					t.Errorf("unexpected reponse: -want, +got: %s", cmp.Diff(tc.wantResponse, string(body)))
				}
			}
		})
	}
}
