package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
)

type fakeAPIRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
}

// fakeAPI answers management API routes from canned responses and records every
// request, so resource logic can be exercised without a real workspace.
type fakeAPI struct {
	// responses is keyed by "METHOD /path". An unlisted route answers 404, which
	// is what makes an unexpected call visible instead of silently passing.
	responses map[string]any
	requests  []fakeAPIRequest
}

func newFakeAPI(t *testing.T, responses map[string]any) (*fakeAPI, *client.ManagementClient) {
	t.Helper()

	fake := &fakeAPI{responses: responses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			return
		}
		fake.requests = append(fake.requests, fakeAPIRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Body:   string(body),
		})

		response, ok := fake.responses[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encoding response for %s %s: %v", r.Method, r.URL.Path, err)
		}
	}))
	t.Cleanup(srv.Close)

	managementClient, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("creating management client: %v", err)
	}
	return fake, managementClient
}

// requestsTo returns every recorded request for one route.
func (f *fakeAPI) requestsTo(method, path string) []fakeAPIRequest {
	var matching []fakeAPIRequest
	for _, request := range f.requests {
		if request.Method == method && request.Path == path {
			matching = append(matching, request)
		}
	}
	return matching
}
