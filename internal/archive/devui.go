package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// devUI serves a checkout's UI against a running Pharos service, so a UI
// change can be tried on a real library without building the app or letting
// branch code open that library's catalog. Pages, assets, and carbon factors
// are read from disk on every request; API calls use the running service with
// its token.
type devUI struct {
	assets string
	token  string
	cookie string
	proxy  *httputil.ReverseProxy
}

func newDevUI(assets string, service *url.URL, token string, port int) *devUI {
	d := &devUI{assets: assets, token: token, cookie: fmt.Sprintf("pharos_dev_token_%d", port)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(service)
			// devUI has already checked the origin; the service sees a
			// bearer-token client, not the browser.
			for _, name := range []string{"Cookie", "Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest"} {
				r.Out.Header.Del(name)
			}
			r.Out.Header.Set("Authorization", "Bearer "+token)
			if r.Out.URL.Path == "/api/health/carbon" || r.Out.URL.Path == "/api/usage/summary" {
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: d.withLocalCarbonFactors,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, map[string]any{"error": fmt.Sprintf("Pharos service at %s is unreachable: %v", service, err)}, http.StatusBadGateway)
		},
	}
	d.proxy = proxy
	return d
}

// The installed service supplies live token counts, while the checkout supplies
// the factors and sources used by the UI being previewed.
func (d *devUI) withLocalCarbonFactors(response *http.Response) error {
	field := ""
	switch response.Request.URL.Path {
	case "/api/health/carbon":
		field = "factors"
	case "/api/usage/summary":
		field = "carbon_factors"
	}
	if field == "" || response.StatusCode != http.StatusOK {
		return nil
	}
	factors, err := os.ReadFile(filepath.Join(d.assets, "carbon", "co2_factors.json"))
	if err != nil {
		return fmt.Errorf("read local carbon factors: %w", err)
	}
	if !json.Valid(factors) {
		return fmt.Errorf("local carbon factors are invalid JSON")
	}
	defer response.Body.Close()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode carbon API response: %w", err)
	}
	body[field] = factors
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode carbon API response: %w", err)
	}
	response.Body = io.NopCloser(bytes.NewReader(payload))
	response.ContentLength = int64(len(payload))
	response.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	response.Header.Del("ETag")
	return nil
}

// ServeHTTP applies the service's own authorization and same-origin rules,
// since requests forwarded from here carry its token.
func (d *devUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && r.URL.Query().Get("token") != "" && secureEqual(r.URL.Query().Get("token"), d.token) {
		http.SetCookie(w, &http.Cookie{Name: d.cookie, Value: url.QueryEscape(d.token), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	cookie, err := r.Cookie(d.cookie)
	if err != nil || !secureEqual(cookie.Value, url.QueryEscape(d.token)) {
		writeJSON(w, map[string]any{"error": "authentication required"}, http.StatusUnauthorized)
		return
	}
	if (r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/api/")) && !sameOrigin(r) {
		writeJSON(w, map[string]any{"error": "cross-origin request refused"}, http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && isUIPage(r.URL.Path):
		source, err := os.ReadFile(filepath.Join(d.assets, "ui.py"))
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		// Tell this tab apart from the installed app's.
		html := strings.Replace(uiHTML(source), "<title>Pharos</title>", "<title>Pharos (dev)</title>", 1)
		writeHTML(w, html)
	case r.Method == http.MethodGet && uiAssets[r.URL.Path].name != "":
		asset := uiAssets[r.URL.Path]
		payload, err := os.ReadFile(filepath.Join(d.assets, asset.name))
		writeAsset(w, asset.name, payload, err, asset.contentType)
	default:
		d.proxy.ServeHTTP(w, r)
	}
}

func runDevUICLI(config Config, args []string) error {
	flags := flag.NewFlagSet("dev-ui", flag.ContinueOnError)
	port := flags.Int("port", 8799, "")
	assets := flags.String("assets", filepath.Join("internal", "archive", "assets"), "")
	noOpen := flags.Bool("no-open", false, "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*assets, "ui.py")); err != nil {
		return fmt.Errorf("no ui.py in %s; run from the repository root or pass --assets DIR", *assets)
	}
	if *port == config.Port {
		return fmt.Errorf("--port %d is the service's own port; choose another", *port)
	}
	service := &url.URL{Scheme: "http", Host: net.JoinHostPort(config.Host, strconv.Itoa(config.Port))}
	if err := pingService(service, config.APIToken); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; API calls will fail until it runs.\n", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	target := fmt.Sprintf("http://127.0.0.1:%d/?token=%s", *port, url.QueryEscape(config.APIToken))
	fmt.Printf("Serving the UI in %s against %s\n%s\n", *assets, service, target)
	handler := newDevUI(*assets, service, config.APIToken, *port)
	if !*noOpen {
		_ = exec.Command("open", target).Run()
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(listener)
}

func pingService(service *url.URL, token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, service.String()+"/api/ready", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("Pharos service at %s is not running", service)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Pharos service at %s answered %s; is this its configuration?", service, response.Status)
	}
	return nil
}
