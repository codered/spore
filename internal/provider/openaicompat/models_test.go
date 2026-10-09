package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestListModelsReadsTheOpenAIShape(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			t.Errorf("%s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"unsloth/b"},{"id":"a"},{"id":""}]}`))
	}))
	defer ts.Close()
	ids, err := New(ts.URL+"/v1", "k", nil).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"a", "unsloth/b"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestListModelsReportsHTTPErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	if _, err := New(ts.URL, "", nil).ListModels(context.Background()); err == nil {
		t.Fatal("want an error for 401")
	}
}
