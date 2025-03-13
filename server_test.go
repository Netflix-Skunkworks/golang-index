package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexHandler(t *testing.T) {
	jan := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	feb := time.Date(2025, 2, 3, 4, 5, 6, 7, time.UTC)
	mar := time.Date(2025, 3, 4, 5, 6, 7, 8, time.UTC)

	tests := []struct {
		name           string
		sinceParam     string
		limitParam     string
		tags           map[string][]*repoTag
		wantStatusCode int
		wantResponse   string
	}{
		{
			name:           "empty response",
			tags:           map[string][]*repoTag{},
			wantStatusCode: http.StatusOK,
			wantResponse:   "",
		},
		{
			name: "response with tags",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: jan,
					},
					{
						tag:        "tag2",
						commitDate: feb,
					},
					{
						tag:        "tag3",
						commitDate: mar,
					},
				},
			},
			wantStatusCode: http.StatusOK,
			wantResponse: "" +
				`{"Path":"github.netflix.net/repo1","Version":"tag1","Timestamp":"2025-01-02T03:04:05Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag3","Timestamp":"2025-03-04T05:06:07Z"}`,
		},
		{
			name:       "with 'since' query param",
			sinceParam: "2025-02-01T00:00:00Z",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: jan,
					},
					{
						tag:        "tag2",
						commitDate: feb,
					},
					{
						tag:        "tag3",
						commitDate: mar,
					},
				},
			},
			wantStatusCode: http.StatusOK,
			wantResponse: "" +
				`{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}` + "\n" +
				`{"Path":"github.netflix.net/repo1","Version":"tag3","Timestamp":"2025-03-04T05:06:07Z"}`,
		},
		{
			name:       "with invalid 'since' query param",
			sinceParam: "invalid",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: jan,
					},
					{
						tag:        "tag2",
						commitDate: feb,
					},
					{
						tag:        "tag3",
						commitDate: mar,
					},
				},
			},
			wantStatusCode: http.StatusBadRequest,
			wantResponse:   "",
		},
		{
			name:       "with 'limit' query param",
			limitParam: "1",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC),
					},
					{
						tag:        "tag2",
						commitDate: time.Date(2025, 2, 3, 4, 5, 6, 7, time.UTC),
					},
					{
						tag:        "tag3",
						commitDate: time.Date(2025, 3, 4, 5, 6, 7, 8, time.UTC),
					},
				},
			},
			wantStatusCode: http.StatusOK,
			wantResponse:   `{"Path":"github.netflix.net/repo1","Version":"tag1","Timestamp":"2025-01-02T03:04:05Z"}`,
		},
		{
			name:       "with invalid 'limt' query param",
			limitParam: "invalid",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC),
					},
					{
						tag:        "tag2",
						commitDate: time.Date(2025, 2, 3, 4, 5, 6, 7, time.UTC),
					},
					{
						tag:        "tag3",
						commitDate: time.Date(2025, 3, 4, 5, 6, 7, 8, time.UTC),
					},
				},
			},
			wantStatusCode: http.StatusBadRequest,
		},
		{
			name:       "with both limit and since query params",
			sinceParam: "2025-02-01T00:00:00Z",
			limitParam: "1",
			tags: map[string][]*repoTag{
				"repo1": []*repoTag{
					{
						tag:        "tag1",
						commitDate: time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC),
					},
					{
						tag:        "tag2",
						commitDate: time.Date(2025, 2, 3, 4, 5, 6, 7, time.UTC),
					},
					{
						tag:        "tag3",
						commitDate: time.Date(2025, 3, 4, 5, 6, 7, 8, time.UTC),
					},
				},
			},
			wantStatusCode: http.StatusOK,
			wantResponse: "" +
				`{"Path":"github.netflix.net/repo1","Version":"tag2","Timestamp":"2025-02-03T04:05:06Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(0, &index{repoTags: tt.tags})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			recorder := httptest.NewRecorder()

			query := request.URL.Query()
			if tt.sinceParam != "" {
				query.Add("since", tt.sinceParam)
			}
			if tt.limitParam != "" {
				query.Add("limit", tt.limitParam)
			}
			request.URL.RawQuery = query.Encode()

			s.handleIndex(recorder, request)

			assert.Equal(t, tt.wantStatusCode, recorder.Code)
			if tt.wantStatusCode == http.StatusOK {
				body, err := io.ReadAll(recorder.Body)
				require.NoError(t, err)
				assert.Equal(t, tt.wantResponse, string(body))
			}
		})
	}
}
