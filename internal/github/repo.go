package github

import (
	"fmt"
	"strings"
)

type repo struct {
	host string
	org  string
	name string
}

func newRepo(hostname, orgRepoName string) (repo, error) {
	parts := strings.Split(orgRepoName, "/")
	if len(parts) != 2 {
		return repo{}, fmt.Errorf("expected org/name format, but got %d parts from %s", len(parts), orgRepoName)
	}

	return repo{host: hostname, org: parts[0], name: parts[1]}, nil
}

func (r repo) fullName() string {
	return fmt.Sprintf("%s/%s", r.org, r.name)
}

func (r repo) asModulePath() string {
	return fmt.Sprintf("%s/%s/%s", r.host, r.org, r.name)
}
