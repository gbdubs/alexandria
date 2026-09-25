package archive

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStateChangingRequestsRefuseOtherOrigins(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	for _, check := range []struct {
		name, origin, site string
		want               int
	}{
		{"page on another local port", "http://127.0.0.1:3000", "same-site", http.StatusForbidden},
		{"other site", "https://example.org", "cross-site", http.StatusForbidden},
		{"opaque origin", "null", "", http.StatusForbidden},
		{"origin without fetch metadata", "http://127.0.0.1:3000", "", http.StatusForbidden},
		{"the UI itself", "http://example.com", "same-origin", http.StatusOK},
		{"non-browser client", "", "", http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/mcp/enabled", strings.NewReader(`{"enabled":true}`))
		request.AddCookie(&http.Cookie{Name: server.cookieName(), Value: config.APIToken})
		if check.origin != "" {
			request.Header.Set("Origin", check.origin)
		}
		if check.site != "" {
			request.Header.Set("Sec-Fetch-Site", check.site)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != check.want {
			t.Errorf("%s: status %d, want %d: %s", check.name, response.Code, check.want, response.Body.String())
		}
	}
}

func TestAPIGetsRefuseOtherOrigins(t *testing.T) {
	catalog, config := testCatalog(t)
	server := NewServer(config, catalog)
	for _, check := range []struct {
		path, site string
		want       int
	}{
		{"/api/sources", "same-site", http.StatusForbidden}, // any API GET, since some act
		{"/api/probe/status", "cross-site", http.StatusForbidden},
		{"/api/sources", "same-origin", http.StatusOK},
		{"/api/sources", "", http.StatusOK}, // non-browser client
		{"/", "cross-site", http.StatusOK},  // a link to the UI still opens it
	} {
		request := httptest.NewRequest(http.MethodGet, check.path, nil)
		request.AddCookie(&http.Cookie{Name: server.cookieName(), Value: config.APIToken})
		if check.site != "" {
			request.Header.Set("Sec-Fetch-Site", check.site)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != check.want {
			t.Errorf("GET %s from %q: status %d, want %d", check.path, check.site, response.Code, check.want)
		}
	}
}
