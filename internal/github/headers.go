package github

import (
	"context"
	"net/http"
)

// HeaderClient returns a client that adds headers to every request it makes.
//
// Entries in headers are sent on every request. ownerHeader, when non-empty,
// names a header whose value is the repository owner the request concerns, taken
// from the request's context; requests made without an owner in context omit it.
// This suits a proxy that selects credentials per organization.
func HeaderClient(client *http.Client, headers map[string]string, ownerHeader string) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	if len(headers) == 0 && ownerHeader == "" {
		return client
	}

	wrapped := *client
	wrapped.Transport = &headerTransport{
		base:        baseTransport(client.Transport),
		headers:     headers,
		ownerHeader: ownerHeader,
	}
	return &wrapped
}

// ContextWithOwner marks ctx as concerning owner, for the header named by
// HeaderClient's ownerHeader.
func ContextWithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerContextKey{}, owner)
}

type ownerContextKey struct{}

type headerTransport struct {
	base        http.RoundTripper
	headers     map[string]string
	ownerHeader string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it is given.
	req = req.Clone(req.Context())
	for name, value := range t.headers {
		req.Header.Set(name, value)
	}
	if t.ownerHeader != "" {
		if owner, ok := req.Context().Value(ownerContextKey{}).(string); ok && owner != "" {
			req.Header.Set(t.ownerHeader, owner)
		}
	}
	return t.base.RoundTrip(req)
}

func baseTransport(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return http.DefaultTransport
	}
	return rt
}
