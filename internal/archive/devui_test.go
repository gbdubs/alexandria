package archive

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevUIServesCheckoutAndForwardsAPIWithToken(t *testing.T) {
	var forwarded *http.Request
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r
		writeJSON(w, map[string]any{"ok": true}, http.StatusOK)
	}))
	defer service.Close()
	target, _ := url.Parse(service.URL)
	assets := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(assets, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ui.py", "APP_HTML = r'''<!doctype html><title>Pharos</title>first'''\n")
	write("library.js", "library()")
	handler := newDevUI(assets, target, "secret", 8799)
	serve := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		request.AddCookie(&http.Cookie{Name: "pharos_dev_token_8799", Value: "secret"})
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/?token=secret", nil))
	if login.Code != http.StatusSeeOther || !strings.Contains(login.Header().Get("Set-Cookie"), "pharos_dev_token_8799=secret") {
		t.Fatalf("token login: %d %q", login.Code, login.Header().Get("Set-Cookie"))
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/library", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("page without cookie: %d", anonymous.Code)
	}

	page := serve(http.MethodGet, "/library", nil)
	if body := page.Body.String(); !strings.HasPrefix(body, "<!doctype html>") || !strings.Contains(body, "<title>Pharos (dev)</title>first") || strings.Contains(body, "EventSource") {
		t.Fatalf("page: %q", body)
	}
	write("ui.py", "APP_HTML = r'''<!doctype html><title>Pharos</title>second'''\n")
	if body := serve(http.MethodGet, "/", nil).Body.String(); !strings.Contains(body, "second") {
		t.Fatalf("edited page not re-read: %q", body)
	}
	if asset := serve(http.MethodGet, "/assets/library.js", nil); asset.Body.String() != "library()" || !strings.HasPrefix(asset.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("asset: %q %q", asset.Body.String(), asset.Header().Get("Content-Type"))
	}
	write("library.js", "libraryUpdated()")
	if asset := serve(http.MethodGet, "/assets/library.js", nil); asset.Body.String() != "libraryUpdated()" || asset.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("refreshed asset: %q cache %q", asset.Body.String(), asset.Header().Get("Cache-Control"))
	}
	if forwarded != nil {
		t.Fatal("pages and assets must not reach the service")
	}

	api := serve(http.MethodPost, "/api/sync?kind=quick", map[string]string{"Origin": "http://evil.test", "Sec-Fetch-Site": "cross-site"})
	if api.Code != http.StatusForbidden {
		t.Fatalf("cross-origin API call: %d", api.Code)
	}
	api = serve(http.MethodPost, "/api/sync?kind=quick", map[string]string{"Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"})
	if api.Code != http.StatusOK || forwarded == nil {
		t.Fatalf("same-origin API call: %d", api.Code)
	}
	if forwarded.URL.Path != "/api/sync" || forwarded.URL.RawQuery != "kind=quick" || forwarded.Method != http.MethodPost {
		t.Fatalf("forwarded %s %s", forwarded.Method, forwarded.URL)
	}
	if forwarded.Header.Get("Authorization") != "Bearer secret" || forwarded.Header.Get("Cookie") != "" || forwarded.Header.Get("Origin") != "" || forwarded.Header.Get("Sec-Fetch-Site") != "" {
		t.Fatalf("forwarded headers: %v", forwarded.Header)
	}

	service.Close()
	if down := serve(http.MethodGet, "/api/health", nil); down.Code != http.StatusBadGateway || !strings.Contains(down.Body.String(), "unreachable") {
		t.Fatalf("service down: %d %s", down.Code, down.Body.String())
	}
}
