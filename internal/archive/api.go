package archive

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SyncRun struct {
	ID               string         `json:"id"`
	Kind             string         `json:"kind"`
	State            string         `json:"state"`
	Phase            string         `json:"phase"`
	Sources          []string       `json:"sources"`
	CurrentSource    any            `json:"current_source"`
	CompletedSources int            `json:"completed_sources"`
	TotalSources     int            `json:"total_sources"`
	Workspaces       int            `json:"workspaces"`
	Conversations    int            `json:"conversations"`
	Messages         int            `json:"messages"`
	SkippedCurrent   int            `json:"skipped_current"`
	Results          []IngestResult `json:"results"`
	Error            any            `json:"error"`
	StartedAt        string         `json:"started_at"`
	UpdatedAt        string         `json:"updated_at"`
	CompletedAt      any            `json:"completed_at"`
}
type Server struct {
	Catalog  *Catalog
	config   Config
	configMu sync.RWMutex
	ingestMu sync.Mutex
	runsMu   sync.RWMutex
	runs     []*SyncRun
	life     lifecycle
	captures captureRuns
	backups  backupRuns
	tasks    backgroundTasks
}

func NewServer(config Config, catalog *Catalog) *Server {
	server := &Server{Catalog: catalog, config: config, runs: []*SyncRun{}}
	server.life.init()
	if catalog != nil {
		catalog.background = server.spawn
	}
	return server
}
func (s *Server) Config() Config { s.configMu.RLock(); defer s.configMu.RUnlock(); return s.config }

func (s *Server) Serve() error {
	config := s.Config()
	if config.Host != "127.0.0.1" && config.Host != "::1" && config.Host != "localhost" {
		return fmt.Errorf("the archive API may bind only to a loopback address")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.Host, strconv.Itoa(config.Port)))
	if err != nil {
		return err
	}
	s.life.onListen()
	display := config.Host
	if strings.Contains(display, ":") {
		display = "[" + display + "]"
	}
	fmt.Printf("Pharos: http://%s:%d/\n", display, config.Port)
	// Requests are cancelled when the service stops.
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return s.life.ctx }}
	s.spawn(s.Catalog.maintainLibrary)
	s.spawn(s.Catalog.maintainSubstringIndex)
	s.spawn(func(ctx context.Context) { s.Catalog.keepWALSmall(ctx, 10*time.Second, walSizeLimit) })
	s.refreshGitInBackground(true)
	return s.serveUntilStopped(server, listener)
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" && r.URL.Query().Get("token") != "" && secureEqual(r.URL.Query().Get("token"), s.Config().APIToken) {
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: url.QueryEscape(r.URL.Query().Get("token")), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.authorized(r) {
		writeJSON(w, map[string]any{"error": "authentication required"}, http.StatusUnauthorized)
		return
	}
	// Some GETs act too (/api/probe runs a login shell, /api/authorship starts
	// a rebuild), so no API call is accepted from another origin.
	if (r.Method != http.MethodGet || strings.HasPrefix(r.URL.Path, "/api/")) && !sameOrigin(r) {
		writeJSON(w, map[string]any{"error": "cross-origin request refused"}, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/release" {
		s.release(w)
		return
	}
	if !s.admit() {
		writeJSON(w, map[string]any{"error": "Pharos is stopping"}, http.StatusServiceUnavailable)
		return
	}
	defer s.life.requests.Done()
	debug.SetPanicOnFault(true) // see exitOnFault
	defer func() {
		if value := recover(); value != nil {
			s.exitIfFault(value)
			writeJSON(w, map[string]any{"error": fmt.Sprint(value)}, http.StatusInternalServerError)
		}
	}()
	if r.Method == http.MethodGet {
		s.get(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.post(w, r)
		return
	}
	writeJSON(w, map[string]any{"error": "method not allowed"}, http.StatusMethodNotAllowed)
}

// sameOrigin refuses requests a browser marks as coming from another origin.
// SameSite cookies do not separate ports, so a page served from any other
// local port could otherwise make the UI's cookie authorize a release, a sync,
// or a source change. Bearer-token clients send neither header.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == "http://"+r.Host
}

func secureEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
func (s *Server) authorized(r *http.Request) bool {
	token := s.Config().APIToken
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && secureEqual(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), token) {
		return true
	}
	// Cookies ignore ports, so each port has its own.
	cookie, err := r.Cookie(s.cookieName())
	if err != nil {
		return false
	}
	value, err := url.QueryUnescape(cookie.Value)
	return err == nil && secureEqual(value, token)
}

// isUIPage reports whether path is one of the app's client-side routes, each
// of which serves the single-page UI.
func isUIPage(path string) bool {
	switch path {
	case "/", "/library", "/settings", "/sources", "/activity", "/usage", "/tools", "/tl1", "/health", "/mcp":
		return true
	}
	return strings.HasPrefix(path, "/work/")
}

// uiAssets maps each static asset URL the UI loads to its file under assets/.
var uiAssets = map[string]struct{ name, contentType string }{
	"/assets/query-tables.js":  {"query-tables.js", "text/javascript; charset=utf-8"},
	"/assets/query-tables.css": {"query-tables.css", "text/css; charset=utf-8"},
	"/assets/onboarding.js":    {"onboarding.js", "text/javascript; charset=utf-8"},
	"/assets/library.js":       {"library.js", "text/javascript; charset=utf-8"},
	"/assets/carbon.js":        {"carbon.js", "text/javascript; charset=utf-8"},
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case isUIPage(path):
		writeHTML(w, appHTML())
	case uiAssets[path].name != "":
		asset := uiAssets[path]
		writeEmbeddedAsset(w, asset.name, asset.contentType)
	case path == "/api/probe":
		writeJSON(w, ProbeSources(s.Config(), s.Catalog), http.StatusOK)
	case path == "/api/probe/status":
		writeJSON(w, probeStatus(s.Config(), s.Catalog), http.StatusOK)
	case strings.HasPrefix(path, "/api/query/"):
		dataset, operation, ok := queryTableRoute(path)
		if !ok {
			writeJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
			return
		}
		s.getQueryTable(w, r, dataset, operation)
	case path == "/api/search":
		options, err := searchOptions(r.URL.Query())
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		options.ctx = r.Context()
		value, err := s.Catalog.Search(options)
		writeResult(w, value, err)
	case path == "/api/library/find":
		q := r.URL.Query()
		value, err := s.Catalog.LibraryFind(r.Context(), LibraryFindOptions{
			Kind: q.Get("kind"), Query: q.Get("q"), Fuzzy: q.Get("fuzzy") != "0",
			CaseSensitive: q.Get("case") == "1", Separators: q.Get("separators") == "1",
			Limit: queryInt(r, "limit", 50), Offset: queryInt(r, "offset", 0),
		})
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
		} else {
			writeJSON(w, value, http.StatusOK)
		}
	case strings.HasPrefix(path, "/api/work/"):
		id := strings.TrimPrefix(path, "/api/work/")
		value, err := s.Catalog.WorkDetail(id)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
		} else if value == nil {
			writeJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
		} else {
			writeJSON(w, value, http.StatusOK)
		}
	case strings.HasPrefix(path, "/api/conversation/"):
		id := strings.TrimPrefix(path, "/api/conversation/")
		limit := queryInt(r, "limit", 200)
		if limit > 1000 {
			limit = 1000
		}
		offset := max(queryInt(r, "offset", 0), 0)
		rows, err := queryMaps(s.Catalog.DB, "SELECT * FROM messages WHERE conversation_id=? ORDER BY source_order IS NULL,source_order,created_at,id LIMIT ? OFFSET ?", id, limit, offset)
		writeResult(w, map[string]any{"items": rows, "limit": limit, "offset": offset}, err)
	case strings.HasPrefix(path, "/api/change/"):
		id := strings.TrimPrefix(path, "/api/change/")
		change, err := queryMaps(s.Catalog.DB, "SELECT * FROM change_sets WHERE id=?", id)
		if err != nil {
			writeError(w, err, 500)
		} else if len(change) == 0 {
			writeJSON(w, map[string]any{"error": "not found"}, 404)
		} else {
			files, e := queryMaps(s.Catalog.DB, "SELECT * FROM change_files WHERE change_set_id=?", id)
			writeResult(w, map[string]any{"change": change[0], "files": files}, e)
		}
	case path == "/api/tl1" || strings.HasPrefix(path, "/api/tl1/"):
		s.getTL1(w, r)
	case path == "/api/ready":
		// The macOS wrapper polls this until the service listens; it must stay
		// cheap, since each timed-out probe would otherwise leave work running.
		writeJSON(w, map[string]any{"ok": true}, 200)
	case path == "/api/health":
		health := s.Catalog.Health()
		health["storage"] = storage(s.Config())
		health["sync_active"] = s.syncActive()
		health["drive"] = s.driveHealth()
		writeJSON(w, health, 200)
	// The Settings page loads each health section separately and renders
	// each as it arrives.
	case path == "/api/health/counts":
		value, err := s.Catalog.HealthCounts(r.Context())
		writeResult(w, value, err)
	case path == "/api/health/pricing":
		value, err := s.Catalog.PricingHealth(r.Context())
		writeResult(w, value, err)
	case path == "/api/health/carbon":
		value, err := s.Catalog.CarbonHealth(r.Context())
		writeResult(w, value, err)
	case path == "/api/health/storage":
		writeJSON(w, merge(storage(s.Config()), map[string]any{"index_bytes": s.Catalog.indexBytes()}), 200)
	case path == "/api/health/freshness":
		writeJSON(w, merge(s.Catalog.Freshness(), map[string]any{"sync_active": s.syncActive()}), 200)
	case path == "/api/authorship":
		value, err := s.Catalog.AuthorshipStats(r.Context())
		writeResult(w, value, err)
	case path == "/api/pricing":
		writeJSON(w, s.Catalog.pricingStatus(), 200)
	case path == "/api/usage/summary":
		value, err := s.Catalog.UsageSummary(r.Context())
		writeResult(w, value, err)
	case path == "/api/tools/status":
		value, err := s.Catalog.ToolLedgerStatus(r.Context())
		writeResult(w, value, err)
	case strings.HasPrefix(path, "/api/tool-calls/"):
		id, _ := url.PathUnescape(strings.TrimPrefix(path, "/api/tool-calls/"))
		value, err := s.Catalog.toolCallDetail(r.Context(), id)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
		} else if value == nil {
			writeJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
		} else {
			writeJSON(w, value, http.StatusOK)
		}
	case path == "/api/sources":
		value, err := s.sourceInventory()
		writeResult(w, value, err)
	case path == "/api/activity":
		writeJSON(w, s.activity(), 200)
	case path == "/api/capture":
		writeJSON(w, s.captureStatus(), http.StatusOK)
	case path == "/api/index":
		writeJSON(w, s.indexStatus(), http.StatusOK)
	case path == "/api/backup":
		writeJSON(w, s.backupStatus(), http.StatusOK)
	case path == "/api/search/status":
		progress, err := s.Catalog.substringIndexProgress(r.Context())
		writeResult(w, map[string]any{"substring": progress}, err)
	case path == "/api/library/status":
		writeJSON(w, s.libraryStatus(), http.StatusOK)
	case path == "/api/health/drive":
		// Just the drive checks, from their cache: cheap enough to poll.
		writeJSON(w, s.driveHealth(), http.StatusOK)
	case path == "/api/mcp":
		value, err := s.Catalog.MCPStatus(s.Config())
		writeResult(w, value, err)
	case path == "/api/mcp/calls":
		value, err := s.Catalog.MCPCallHistory(queryInt(r, "limit", 50), queryInt(r, "offset", 0), r.URL.Query().Get("tool"), r.URL.Query().Get("status"))
		writeResult(w, value, err)
	case strings.HasPrefix(path, "/api/receipt/"):
		id := strings.TrimPrefix(path, "/api/receipt/")
		rows, err := queryMaps(s.Catalog.DB, "SELECT * FROM receipts WHERE id=? OR operation_id=?", id, id)
		if err != nil {
			writeError(w, err, 500)
		} else if len(rows) == 0 {
			writeJSON(w, map[string]any{"error": "not found"}, 404)
		} else {
			writeJSON(w, rows[0], 200)
		}
	case path == "/api/trace":
		options, err := searchOptions(r.URL.Query())
		if err != nil {
			writeError(w, err, 400)
			return
		}
		options.Query = ""
		value, err := s.Catalog.Search(options)
		writeResult(w, value, err)
	default:
		writeJSON(w, map[string]any{"error": "not found"}, 404)
	}
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/query/") {
		dataset, operation, ok := queryTableRoute(r.URL.Path)
		if !ok {
			writeJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
			return
		}
		s.postQueryTable(w, r, dataset, operation)
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1_000_000))
	if err := decoder.Decode(&body); err != nil && err.Error() != "EOF" {
		writeError(w, err, 400)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/tools/backfill":
		// Building the ledger re-reads every retained conversation, so it runs
		// in the background; progress is reported by /api/tools/status. A
		// release stops it between conversations; the next build resumes.
		started := s.spawn(func(ctx context.Context) {
			if _, err := s.Catalog.BackfillToolLedger(ctx, nil); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "Tool ledger build: %v\n", err)
			}
			if err := s.Catalog.ensureToolRollup(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "Tool rollup: %v\n", err)
			}
		})
		if !started {
			writeJSON(w, map[string]any{"error": "Pharos is stopping"}, http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"started": true}, http.StatusAccepted)
	case path == "/api/capture":
		s.startCapture(w, body)
	case path == "/api/index":
		s.startIndex(w, body)
	case path == "/api/backup":
		s.startBackup(w, body)
	case path == "/api/backup/cancel":
		writeJSON(w, map[string]any{"cancelled": s.backups.stop()}, http.StatusOK)
	case path == "/api/mcp/rpc":
		s.mcpRPC(w, r, body)
	case path == "/api/mcp/enabled":
		enabled, ok := body["enabled"].(bool)
		if !ok {
			writeError(w, fmt.Errorf("enabled must be true or false"), http.StatusBadRequest)
			return
		}
		if err := s.Catalog.SetMCPEnabled(enabled); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"enabled": enabled}, http.StatusOK)
	case path == "/api/probe/accept":
		s.acceptProbe(w, body)
	case path == "/api/sources/sync":
		config := s.Config()
		sources := []SourceConfig{}
		for _, source := range config.Sources {
			if source.Enabled {
				sources = append(sources, source)
			}
		}
		s.syncSources(w, sources)
	case strings.HasPrefix(path, "/api/sources/") && strings.HasSuffix(path, "/sync"):
		name, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/api/sources/"), "/sync"))
		source, err := s.configuredSource(strings.TrimSuffix(name, "/"))
		if err != nil {
			writeError(w, err, 400)
			return
		}
		s.syncSources(w, []SourceConfig{source})
	case strings.HasPrefix(path, "/api/sources/") && strings.HasSuffix(path, "/enabled"):
		name, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/api/sources/"), "/enabled"))
		name = strings.TrimSuffix(name, "/")
		source, err := s.configuredSource(name)
		if err != nil {
			writeError(w, err, 400)
			return
		}
		enabled, ok := body["enabled"].(bool)
		if !ok {
			writeError(w, fmt.Errorf("enabled must be true or false"), 400)
			return
		}
		if s.Config().Path == "" {
			writeError(w, fmt.Errorf("the running service has no writable configuration path"), 400)
			return
		}
		if err := s.setSourceEnabled(source.Name, enabled); err != nil {
			writeError(w, err, 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "source": name, "enabled": enabled}, 200)
	default:
		writeJSON(w, map[string]any{"error": "not found"}, 404)
	}
}

func searchOptions(query url.Values) (SearchOptions, error) {
	options := SearchOptions{Query: query.Get("q"), Repository: query.Get("repository"), Source: query.Get("source"), File: query.Get("file"), Owner: query.Get("owner"), Provider: query.Get("provider"), Model: query.Get("model"), From: query.Get("from"), To: query.Get("to"), Flavor: query.Get("flavor"), Version: query.Get("version"), Outcome: query.Get("outcome"), Error: query.Get("error"), Metric: query.Get("metric"), ChangedOnly: query.Get("changed") == "1", Substring: query.Get("substring") == "1", Limit: 50}
	if query.Get("limit") != "" {
		value, err := strconv.Atoi(query.Get("limit"))
		if err != nil {
			return options, err
		}
		options.Limit = value
	}
	if query.Get("offset") != "" {
		value, err := strconv.Atoi(query.Get("offset"))
		if err != nil {
			return options, err
		}
		options.Offset = value
	}
	if query.Get("pr") != "" {
		value, err := strconv.Atoi(query.Get("pr"))
		if err != nil {
			return options, err
		}
		options.PR = &value
	}
	if query.Get("min") != "" {
		value, err := strconv.ParseFloat(query.Get("min"), 64)
		if err != nil {
			return options, err
		}
		options.Minimum = &value
	}
	if query.Get("max") != "" {
		value, err := strconv.ParseFloat(query.Get("max"), 64)
		if err != nil {
			return options, err
		}
		options.Maximum = &value
	}
	return options, nil
}
func queryInt(r *http.Request, key string, fallback int) int {
	if r.URL.Query().Get(key) == "" {
		return fallback
	}
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	return value
}

func (s *Server) configuredSource(name string) (SourceConfig, error) {
	for _, source := range s.Config().Sources {
		if source.Name == name {
			return source, nil
		}
	}
	return SourceConfig{}, fmt.Errorf("configured source not found: %s", name)
}

// setSourceEnabled writes a toggle to the file defining the source and
// applies it to the current running config, under the lock reloadSources
// takes, so neither loses the other's change. The change is copy-on-write:
// every Config() copy shares its Sources array with readers in other
// requests. hostFileMu keeps a probe accept's read-modify-write of the host
// file from dropping it.
func (s *Server) setSourceEnabled(name string, enabled bool) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	index := slices.IndexFunc(s.config.Sources, func(source SourceConfig) bool { return source.Name == name })
	if index < 0 {
		return fmt.Errorf("configured source not found: %s", name)
	}
	hostFileMu.Lock()
	// A library host's own sources live in its hosts/<id>.toml.
	err := SetSourceEnabled(defaultString(s.config.Sources[index].File, s.config.Path), name, enabled)
	hostFileMu.Unlock()
	if err != nil {
		return err
	}
	sources := slices.Clone(s.config.Sources)
	sources[index].Enabled = enabled
	s.config.Sources = sources
	return nil
}

func (s *Server) sourceInventory() (map[string]any, error) {
	var ingestedDocuments, totalConversations int
	if err := s.Catalog.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM conversation_documents),
		(SELECT COUNT(*) FROM conversations)`).Scan(&ingestedDocuments, &totalConversations); err != nil {
		return nil, err
	}
	freshness := s.Catalog.Freshness()
	states := map[string]map[string]any{}
	if rows, ok := freshness["sources"].([]map[string]any); ok {
		for _, row := range rows {
			states[firstString(row["source_name"])] = row
		}
	}
	// Configured sources are this host's; other hosts' sync state is reported
	// alongside without being matched to this host's paths.
	host := currentHost()
	hosts, err := s.Catalog.Hosts()
	if err != nil {
		return nil, err
	}
	config := s.Config()
	items := []map[string]any{}
	enabled := 0
	for _, source := range config.Sources {
		state := states[source.Name]
		pathStatus := "unconfigured"
		if source.Path != "" {
			if _, err := os.Stat(source.Path); err == nil {
				pathStatus = "available"
			} else {
				pathStatus = "missing"
			}
		}
		capability := "retrieval-only"
		if adapter, err := MakeAdapter(source); err == nil {
			capability = adapter.Capability()
		}
		if firstString(state["capability"]) != "" {
			capability = firstString(state["capability"])
		}
		coverage := defaultString(state["coverage"], "not-indexed")
		stale := true
		if value, ok := state["stale"].(bool); ok {
			stale = value
		}
		item := map[string]any{"name": source.Name, "kind": source.Kind, "account": source.Account, "path": nilIfEmpty(source.Path), "path_status": pathStatus, "enabled": source.Enabled, "capability": capability, "coverage": coverage, "last_attempt_at": state["last_attempt_at"], "last_success_at": state["last_success_at"], "pending_count": integer(state["pending_count"]), "error": state["error"], "stale": stale, "host_id": host.ID, "host_label": host.Label}
		items = append(items, item)
		if source.Enabled {
			enabled++
		}
	}
	return map[string]any{"items": items, "configured": len(items), "enabled": enabled,
		"host": host, "hosts": hosts, "other_host_sources": freshness["other_host_sources"],
		"ingested_documents": ingestedDocuments, "total_conversations": totalConversations,
		"note": "Source indexing reads files without changing them."}, nil
}

func (s *Server) syncSources(w http.ResponseWriter, sources []SourceConfig) {
	if !s.ingestMu.TryLock() {
		writeJSON(w, map[string]any{"error": "source indexing is already running"}, http.StatusConflict)
		return
	}
	defer s.ingestMu.Unlock()
	run := s.startRun(sources)
	results := []IngestResult{}
	ctx := s.life.ctx
	for index, source := range sources {
		if ctx.Err() != nil {
			break
		}
		s.updateRun(run.ID, func(run *SyncRun) {
			run.CurrentSource = source.Name
			run.Phase = "checking"
			run.CompletedSources = index
		})
		adapter, err := syncAdapter(source)
		if err != nil {
			result := IngestResult{Source: source.Name, Error: err.Error()}
			results = append(results, result)
			continue
		}
		baseW, baseC, baseM := totals(results)
		result := s.Catalog.IngestContext(ctx, adapter, func(phase string, w, c, m, skipped int) {
			s.updateRun(run.ID, func(run *SyncRun) {
				run.CurrentSource = source.Name
				run.Phase = phase
				run.Workspaces = baseW + w
				run.Conversations = baseC + c
				run.Messages = baseM + m
				run.SkippedCurrent = skippedTotals(results) + skipped
			})
		})
		results = append(results, result)
		tw, tc, tm := totals(results)
		s.updateRun(run.ID, func(run *SyncRun) {
			run.Results = append([]IngestResult{}, results...)
			run.CompletedSources = index + 1
			run.Workspaces = tw
			run.Conversations = tc
			run.Messages = tm
			run.SkippedCurrent = skippedTotals(results)
		})
	}
	links, linkErr := 0, error(nil)
	if ctx.Err() == nil {
		links, linkErr = s.Catalog.ReconcileIdentities()
	}
	ok := linkErr == nil && ctx.Err() == nil
	var firstError any
	for _, result := range results {
		if result.Error != nil {
			ok = false
			if firstError == nil {
				firstError = result.Error
			}
		}
	}
	if linkErr != nil {
		firstError = linkErr.Error()
	}
	s.updateRun(run.ID, func(run *SyncRun) {
		if ok {
			run.State = "complete"
			run.Phase = "complete"
		} else if ctx.Err() != nil {
			run.State = "interrupted"
			run.Phase = "interrupted"
		} else {
			run.State = "failed"
			run.Phase = "failed"
		}
		run.CurrentSource = nil
		run.CompletedAt = now()
		run.Error = firstError
	})
	writeJSON(w, map[string]any{"ok": ok, "run_id": run.ID, "results": results, "identity_links_added": links}, 200)
	// Send the result, then fold what the sync wrote into the database file
	// before another sync can start (see keepWALSmall).
	if flusher, canFlush := w.(http.Flusher); canFlush {
		flusher.Flush()
	}
	_ = s.Catalog.Checkpoint()
	// Git ancestry enrichment can involve thousands of local Git calls. Run it
	// once after the entire source sync, without holding up ingestion progress.
	// It also rebuilds the Tools rollup (see refreshGitInBackground).
	s.refreshGitInBackground(false)
}
func totals(results []IngestResult) (int, int, int) {
	w, c, m := 0, 0, 0
	for _, item := range results {
		w += item.Workspaces
		c += item.Conversations
		m += item.Messages
	}
	return w, c, m
}

func skippedTotals(results []IngestResult) int {
	total := 0
	for _, result := range results {
		total += result.SkippedCurrent
	}
	return total
}
func (s *Server) startRun(sources []SourceConfig) *SyncRun {
	names := []string{}
	for _, source := range sources {
		names = append(names, source.Name)
	}
	return s.startRunNamed("source-sync", names)
}

func (s *Server) startRunNamed(kind string, names []string) *SyncRun {
	timestamp := now()
	run := &SyncRun{ID: randomToken(), Kind: kind, State: "running", Phase: "starting", Sources: names, TotalSources: len(names), Results: []IngestResult{}, StartedAt: timestamp, UpdatedAt: timestamp}
	s.runsMu.Lock()
	s.runs = append([]*SyncRun{run}, s.runs...)
	if len(s.runs) > 20 {
		s.runs = s.runs[:20]
	}
	s.runsMu.Unlock()
	return run
}
func (s *Server) updateRun(id string, update func(*SyncRun)) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	for _, run := range s.runs {
		if run.ID == id {
			update(run)
			run.UpdatedAt = now()
			return
		}
	}
}
func (s *Server) activity() map[string]any {
	s.runsMu.RLock()
	defer s.runsMu.RUnlock()
	data, _ := json.Marshal(s.runs)
	var runs []map[string]any
	_ = json.Unmarshal(data, &runs)
	active := 0
	for _, run := range runs {
		if firstString(run["state"]) == "running" {
			active++
		}
	}
	return map[string]any{"runs": runs, "active": active}
}

func (s *Server) syncActive() bool {
	s.runsMu.RLock()
	defer s.runsMu.RUnlock()
	for _, run := range s.runs {
		if run.State == "running" {
			return true
		}
	}
	return false
}

func storage(config Config) map[string]any {
	internalTotal, internalFree := diskUsage(filepath.Dir(config.CatalogPath))
	common := map[string]any{"internal_total_bytes": internalTotal, "internal_free_bytes": internalFree, "staging_bytes": treeSize(config.StagingRoot), "staging_cap_bytes": config.StagingCapBytes, "volume_name": volumeName(config.ArchiveRoot)}
	if _, err := os.Stat(config.ArchiveRoot); err != nil {
		return merge(map[string]any{"status": "disconnected", "path": config.ArchiveRoot}, common)
	}
	total, free := diskUsage(config.ArchiveRoot)
	identity := cachedVolumeIdentity(config.ArchiveRoot)
	status := "available"
	if config.VolumeID != "" && identity != config.VolumeID {
		status = "wrong-volume"
	}
	return merge(map[string]any{"status": status, "path": config.ArchiveRoot, "volume_id": identity, "total_bytes": total, "free_bytes": free}, common)
}
func volumeName(path string) string {
	clean := filepath.Clean(path)
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) >= 2 && parts[0] == "Volumes" {
		return parts[1]
	}
	return "Internal disk"
}
func merge(left, right map[string]any) map[string]any {
	for key, value := range right {
		left[key] = value
	}
	return left
}

func writeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err, 500)
	} else {
		writeJSON(w, value, 200)
	}
}
func writeError(w http.ResponseWriter, err error, status int) {
	writeJSON(w, map[string]any{"error": err.Error()}, status)
}
func writeJSON(w http.ResponseWriter, value any, status int) {
	payload, err := json.Marshal(value)
	if err != nil {
		status = 500
		payload = []byte(`{"error":"response encoding failed"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func writeEmbeddedAsset(w http.ResponseWriter, name, contentType string) {
	payload, err := queryTableAsset(name)
	writeAsset(w, name, payload, err, contentType)
}
func writeAsset(w http.ResponseWriter, name string, payload []byte, err error, contentType string) {
	if err != nil {
		writeError(w, fmt.Errorf("asset unavailable: %s", name), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}
func writeHTML(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// query-table evaluates computed columns in a Blob-backed Web Worker. Keep
	// every network-capable directive same-origin while allowing that isolated
	// worker; child-src is included for older WebKit worker CSP handling.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; worker-src 'self' blob:; child-src 'self' blob:")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(value))
}
