package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
)

// modelsServer is newTestServer plus a models service over two providers: one
// serving /v1/models and one that is unreachable.
func modelsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, ts := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma"},{"id":"unsloth/qwen"}]}`))
	}))
	t.Cleanup(up.Close)
	reg := provider.NewRegistry()
	reg.Register("jetson", openaicompat.New(up.URL+"/v1", "", nil), provider.ProviderPrice{})
	reg.Register("down", openaicompat.New("http://127.0.0.1:1/v1", "", nil), provider.ProviderPrice{})
	rt, err := router.New(nil, "jetson/gemma")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("default_model = \"jetson/gemma\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.AttachModels(&models.Service{Store: srv.store, Router: rt, Catalog: models.NewCatalog(reg, nil), ConfigPath: cfgPath})
	return ts, createTestSession(t, ts.URL)
}

func decodeModels(t *testing.T, res *http.Response) ModelsJSON {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", res.Status)
	}
	var v ModelsJSON
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func put(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGetModelsListsOpsAndGroups(t *testing.T) {
	ts, id := modelsServer(t)
	res, err := http.Get(ts.URL + "/api/models?session=" + id)
	if err != nil {
		t.Fatal(err)
	}
	v := decodeModels(t, res)
	if len(v.Ops) != 6 || v.Ops[0].Op != "chat" {
		t.Fatalf("ops = %+v", v.Ops)
	}
	var sawDown, sawQwen bool
	for _, g := range v.Groups {
		if g.Provider == "down" && g.Error != "" {
			sawDown = true
		}
		for _, r := range g.Refs {
			if r == "jetson/unsloth/qwen" {
				sawQwen = true
			}
		}
	}
	if !sawDown || !sawQwen {
		t.Fatalf("groups = %+v", v.Groups)
	}
}

func TestPutSessionModelAndRouting(t *testing.T) {
	ts, id := modelsServer(t)
	v := decodeModels(t, put(t, ts.URL+"/api/sessions/"+id+"/model", map[string]string{"op": "chat", "ref": "jetson/unsloth/qwen"}))
	if v.Ops[0].Selected != "jetson/unsloth/qwen" {
		t.Fatalf("chat = %+v", v.Ops[0])
	}
	v = decodeModels(t, put(t, ts.URL+"/api/routing?session="+id, map[string]string{"op": "title", "ref": "jetson/unsloth/qwen"}))
	for _, o := range v.Ops {
		if o.Op == "title" && o.Selected != "jetson/unsloth/qwen" {
			t.Fatalf("title = %+v", o)
		}
	}
}

func TestPutRefusesWrongScopeAndUnavailableModels(t *testing.T) {
	ts, id := modelsServer(t)
	for _, tc := range []struct {
		url  string
		body map[string]string
	}{
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "title", "ref": "jetson/gemma"}},
		{ts.URL + "/api/routing", map[string]string{"op": "chat", "ref": "jetson/gemma"}},
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "chat", "ref": "jetson/nope"}},
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "chat", "ref": "down/anything"}},
	} {
		res := put(t, tc.url, tc.body)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s %v: status = %d, want 400", tc.url, tc.body, res.StatusCode)
		}
	}
	res := put(t, ts.URL+"/api/sessions/nope/model", map[string]string{"op": "chat", "ref": "jetson/gemma"})
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", res.StatusCode)
	}
}

func TestModelsWithoutAServiceIsUnavailable(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
