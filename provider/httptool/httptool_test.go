package httptool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloverzhang/agent-kit/capability"
)

func TestHTTPToolParamDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path":  r.URL.Path,
			"query": r.URL.RawQuery,
			"body":  string(body),
		})
	}))
	defer srv.Close()

	c, err := New("api", Config{
		Name:   "update_city",
		Method: "POST",
		URL:    srv.URL + "/cities/{id}",
		Params: map[string]Param{
			"id":     {Type: "string", In: "path", Required: true},
			"notify": {Type: "boolean", In: "query"},
			"name":   {Type: "string", In: "body"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// POST 未显式声明 risk → 应推断为 mutating
	if c.Meta().Risk != capability.RiskMutating {
		t.Fatalf("risk = %v", c.Meta().Risk)
	}

	out, err := capability.Invoke(context.Background(), c, `{"id":"bj","notify":true,"name":"北京"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "/cities/bj") || !strings.Contains(out, "notify=true") || !strings.Contains(out, "北京") {
		t.Fatalf("param dispatch failed: %s", out)
	}
}

func TestHTTPToolErrorReturnedToModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New("api", Config{Name: "q", URL: srv.URL})
	out, err := capability.Invoke(context.Background(), c, "{}")
	if err != nil {
		t.Fatalf("HTTP 4xx should be returned to model, not as error: %v", err)
	}
	if !strings.Contains(out, "HTTP 404") {
		t.Fatalf("got %q", out)
	}
}
