package archive

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// An agent client keeps its MCP server process for the whole session, often
// days, while the library's drive may be ejected, unplugged, and carried to
// another Mac. So the server never holds the catalog between requests (an open
// file keeps Finder from ejecting the drive) and never fails for good. Each
// tools/list and tools/call reloads the configuration, then goes to the running
// Pharos service for it, whose open catalog and warm Library cache answer and
// record the call, or, when no service answers, opens the catalog for that one
// request. While the library is unavailable, initialize and tools/list still
// answer and tool calls fail with a "not connected" error; the next call after
// the drive returns succeeds.
type mcpServer struct {
	configPath  string // --config; "" resolves as LoadConfig does
	libraryJSON string // --library-json, used when configPath is ""
	identity    func(string) string
	client      *http.Client
	verified    string // library directory last checked against volume_id
	// answer is handleMCP and restart restartMCP; tests replace them.
	answer  func(*Catalog, map[string]any) map[string]any
	restart func(pending []byte) error
	faulted bool // a direct call hit a memory fault (see direct)
}

func newMCPServer(configPath, libraryJSON string) *mcpServer {
	// No proxy: the request carries the API token.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, IdleConnTimeout: 30 * time.Second}
	// Resolved now: the launcher starts runtime/current/..., which a newer
	// build may repoint, and a restart must not switch builds mid-session.
	executable, err := os.Executable()
	if resolved, resolveErr := filepath.EvalSymlinks(executable); err == nil && resolveErr == nil {
		executable = resolved
	}
	return &mcpServer{configPath: configPath, libraryJSON: libraryJSON, identity: volumeIdentity,
		client: &http.Client{Transport: transport, Timeout: 2 * time.Minute}, answer: handleMCP,
		restart: func(pending []byte) error { return restartMCP(executable, pending) }}
}

// RunMCP serves MCP over stdio for the library that configPath or, without
// one, the library.json at libraryJSON names.
func RunMCP(configPath, libraryJSON string, input io.Reader, output io.Writer) error {
	quickHostDetection = true
	return newMCPServer(configPath, libraryJSON).run(input, output)
}

// mcpPendingEnv hands input read ahead but not yet answered to the process
// that restartMCP replaces this one with.
const mcpPendingEnv = "PHAROS_MCP_PENDING"

func (m *mcpServer) run(input io.Reader, output io.Writer) error {
	if _, err := m.config(); err != nil {
		fmt.Fprintf(os.Stderr, "Pharos MCP: %v; tool calls fail until it is available\n", err)
	}
	if pending, ok := os.LookupEnv(mcpPendingEnv); ok {
		os.Unsetenv(mcpPendingEnv)
		data, _ := base64.StdEncoding.DecodeString(pending)
		input = io.MultiReader(bytes.NewReader(data), input)
	}
	return serveMCP(input, output, m.handle, m.afterAnswer)
}

// afterAnswer restarts the process once the answer to a faulted call is out.
func (m *mcpServer) afterAnswer(pending []byte) {
	if !m.faulted {
		return
	}
	err := m.restart(pending)
	fmt.Fprintf(os.Stderr, "Pharos MCP: could not restart after a memory fault, so calls on the same catalog may fault again: %v\n", err)
	m.faulted = false
}

// restartMCP replaces this process with a fresh run of executable, this
// process's own. The PID and stdio stay, so the agent client stays connected.
// It returns only on failure.
func restartMCP(executable string, pending []byte) error {
	fmt.Fprintln(os.Stderr, "Pharos MCP: restarting after a memory fault while reading the catalog")
	return syscall.Exec(executable, os.Args, append(os.Environ(), mcpPendingEnv+"="+base64.StdEncoding.EncodeToString(pending)))
}

func (m *mcpServer) handle(request map[string]any) map[string]any {
	method := firstString(request["method"])
	if method != "tools/list" && method != "tools/call" {
		return handleMCP(nil, request)
	}
	response, err := m.dispatch(request)
	switch {
	case err == nil:
		return response
	case method == "tools/list":
		// Clients keep the tools listed when they connect, so list them while
		// the library is away too; calls then say why they fail.
		return mcpResult(request, map[string]any{"tools": mcpTools})
	default:
		return mcpToolError(request, err.Error())
	}
}

func (m *mcpServer) dispatch(request map[string]any) (map[string]any, error) {
	config, err := m.config()
	if err != nil {
		return nil, err
	}
	if response, forwarded, err := m.forward(config, request); forwarded {
		return response, err
	}
	return m.direct(config, request)
}

func (m *mcpServer) config() (Config, error) {
	path, volume := m.configPath, ""
	if path == "" && m.libraryJSON != "" {
		pointer, err := readLibraryPointer(m.libraryJSON)
		if err != nil {
			m.verified = ""
			return Config{}, err
		}
		path, volume = filepath.Join(pointer.LibraryDir, libraryConfigName), pointer.VolumeUUID
	}
	config, err := loadConfig(path, false)
	if err != nil {
		m.verified = ""
		if _, statErr := os.Stat(config.Path); errors.Is(statErr, os.ErrNotExist) {
			return config, libraryNotConnected(config.Path, volume)
		}
	}
	return config, err
}

// libraryNotConnected explains a missing library file. A drive that is not
// mounted leaves no /Volumes/NAME behind.
func libraryNotConnected(path, volumeUUID string) error {
	reason := path + " is missing"
	if rest, ok := strings.CutPrefix(path, "/Volumes/"); ok {
		mount := "/Volumes/" + strings.SplitN(rest, "/", 2)[0]
		if _, err := os.Stat(mount); errors.Is(err, os.ErrNotExist) {
			reason = mount + " is not mounted"
		}
	}
	if volumeUUID = strings.TrimPrefix(volumeUUID, "uuid:"); volumeUUID != "" {
		reason += " (volume " + volumeUUID + ")"
	}
	return fmt.Errorf("Pharos library is not connected: %s", reason)
}

// mcpCatalogHeader names the catalog a forwarded request is for, so that a
// service with another catalog open on the same port declines it.
const mcpCatalogHeader = "X-Pharos-Catalog"

// forward sends request to the running service for config. It reports false
// only when nothing ran there, so the caller may open the catalog itself.
func (m *mcpServer) forward(config Config, request map[string]any) (map[string]any, bool, error) {
	if config.Host != "127.0.0.1" && config.Host != "::1" && config.Host != "localhost" {
		return nil, false, nil
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, true, err
	}
	post, err := http.NewRequest(http.MethodPost, "http://"+net.JoinHostPort(config.Host, strconv.Itoa(config.Port))+"/api/mcp/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, false, nil
	}
	post.Header.Set("Authorization", "Bearer "+config.APIToken)
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set(mcpCatalogHeader, config.CatalogPath)
	answer, err := m.client.Do(post)
	if err != nil {
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			return nil, false, nil
		}
		// The service may have run the call. Rerunning it here could also open
		// a catalog the service just released for an eject.
		return nil, true, fmt.Errorf("the Pharos service did not answer: %w", err)
	}
	defer answer.Body.Close()
	switch answer.StatusCode {
	case http.StatusOK:
		var response map[string]any
		decoder := json.NewDecoder(answer.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&response); err != nil {
			return nil, true, fmt.Errorf("the Pharos service sent an unreadable answer: %w", err)
		}
		return response, true, nil
	case http.StatusNoContent:
		return nil, true, nil
	case http.StatusServiceUnavailable:
		return nil, true, errLibraryReleased
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict:
		// Another library's service, or a build without forwarding.
		_, _ = io.Copy(io.Discard, answer.Body)
		return nil, false, nil
	}
	return nil, true, fmt.Errorf("the Pharos service answered %s", answer.Status)
}

// direct opens the catalog for this request alone.
//
// A drive yanked mid-call faults on SQLite's mapped index (see exitOnFault).
// The call then fails, and the process restarts after answering (see
// afterAnswer): SQLite's state for the catalog is unusable, and it would be
// reused for the same file (same device and inode) once the drive is back.
// So after a fault nothing touches the catalog again, not even to close it.
func (m *mcpServer) direct(config Config, request map[string]any) (response map[string]any, err error) {
	if err := m.checkLibrary(config); err != nil {
		return nil, err
	}
	if releasedRecently(config.CatalogPath) {
		return nil, errLibraryReleased
	}
	defer debug.SetPanicOnFault(debug.SetPanicOnFault(true))
	defer func() {
		if value := recover(); value != nil {
			if _, fault := value.(interface{ Addr() uintptr }); !fault {
				panic(value)
			}
			m.faulted, m.verified = true, ""
			response, err = nil, libraryFault(config.CatalogPath, value)
		}
	}()
	catalog, err := OpenCatalogForQuery(config.CatalogPath)
	if errors.Is(err, os.ErrNotExist) {
		if config.Library {
			m.verified = ""
			return nil, libraryNotConnected(config.CatalogPath, "")
		}
		// Only a library refuses to start a missing catalog (see checkLibrary).
		if err = config.EnsureDirs(); err == nil {
			catalog, err = OpenCatalog(config.CatalogPath)
		}
	}
	if err != nil {
		return nil, err
	}
	response = m.answer(catalog, request)
	catalog.closeQuery()
	return response, nil
}

func libraryFault(catalogPath string, value any) error {
	dir := filepath.Dir(catalogPath)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("Pharos library disconnected: its drive went away during this call (%s is gone); calls succeed again once it is back", dir)
	}
	return fmt.Errorf("Pharos library disconnected: reading %s failed with a memory fault (%v); try again", catalogPath, value)
}

var errLibraryReleased = errors.New("Pharos library is not connected: the Pharos service has released it so that its drive can be ejected")

// A release (before an eject) leaves releasedMarkerName beside the catalog.
// For releaseQuiet, or until the next serve removes it, the direct path does
// not reopen the catalog: a call between the service exiting and the drive
// unmounting would make the approved eject fail.
const (
	releasedMarkerName = ".released"
	releaseQuiet       = 30 * time.Second
)

func releasedMarker(catalogPath string) string {
	return filepath.Join(filepath.Dir(catalogPath), releasedMarkerName)
}

func releasedRecently(catalogPath string) bool {
	info, err := os.Stat(releasedMarker(catalogPath))
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime())
	return age > -releaseQuiet && age < releaseQuiet
}

// checkLibrary runs the library guards before a direct open. Checking the
// volume shells out to diskutil (~0.1 s), so it is skipped while the library
// directory is the same one on the same device as when it last passed.
func (m *mcpServer) checkLibrary(config Config) error {
	identity := m.identity
	key := ""
	if info, err := os.Stat(filepath.Dir(config.Path)); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			key = fmt.Sprint(config.Path, "|", config.VolumeID, "|", stat.Dev, "|", stat.Ino)
		}
	}
	if key != "" && key == m.verified {
		identity = func(string) string { return config.VolumeID }
	}
	m.verified = ""
	if err := config.checkLibrary(identity); err != nil {
		return err
	}
	m.verified = key
	return nil
}

// mcpRPC answers an MCP request that an agent's MCP server process forwarded,
// from this service's catalog, recording history as that process would.
func (s *Server) mcpRPC(w http.ResponseWriter, r *http.Request, request map[string]any) {
	catalog := s.Catalog
	if catalog == nil {
		writeJSON(w, map[string]any{"error": "the library is not open"}, http.StatusServiceUnavailable)
		return
	}
	if path := r.Header.Get(mcpCatalogHeader); path != "" && !sameFile(path, catalog.Path) {
		writeJSON(w, map[string]any{"error": "this service has a different catalog open"}, http.StatusConflict)
		return
	}
	response := handleMCP(catalog, request)
	if response == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, response, http.StatusOK)
}

func sameFile(left, right string) bool {
	a, err := os.Stat(left)
	if err != nil {
		return false
	}
	b, err := os.Stat(right)
	return err == nil && os.SameFile(a, b)
}

// pharosSupportDir holds per-Mac state that must not live on the library
// drive: the local copy of the app (runtime/), library.json, and the MCP
// launcher. PHAROS_SUPPORT_DIR overrides it.
func pharosSupportDir() string {
	if dir := os.Getenv("PHAROS_SUPPORT_DIR"); dir != "" {
		if absolute, err := filepath.Abs(expandPath(dir)); err == nil {
			return absolute
		}
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Pharos")
}

// libraryPointer is <support>/library.json, which the Pharos app writes when
// it runs from a library, and the service when it serves one (recordLibrary):
//
//	{"library_dir": "/Volumes/euclid/Pharos", "volume_uuid": "642C2C39-…", "updated_at": "…"}
//
// library_dir, the absolute directory holding library.toml, is required.
// volume_uuid (bare or "uuid:"-prefixed) only names the drive in messages;
// library.toml's volume_id is what the guard checks. Other fields are ignored.
type libraryPointer struct {
	LibraryDir string `json:"library_dir"`
	VolumeUUID string `json:"volume_uuid"`
}

func readLibraryPointer(path string) (libraryPointer, error) {
	var pointer libraryPointer
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pointer, fmt.Errorf("Pharos library is not connected: no library has been opened on this Mac yet (%s is missing); open Pharos from its library drive", path)
	} else if err != nil {
		return pointer, err
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return pointer, fmt.Errorf("read %s: %w", path, err)
	}
	if !filepath.IsAbs(pointer.LibraryDir) {
		return pointer, fmt.Errorf("%s: library_dir must be an absolute path, not %q", path, pointer.LibraryDir)
	}
	return pointer, nil
}

// recordLibrary writes <support>/library.json for a library being served,
// with the fields the app's runtime copy writes, so the MCP launcher also
// finds a library the app runs in place or the CLI serves. volume names the
// external volume holding a directory ("" for the startup disk).
func recordLibrary(support string, config Config, volume func(dir string) (uuid, name string)) error {
	dir := filepath.Dir(config.Path)
	uuid, name := volume(dir)
	if pin, ok := strings.CutPrefix(config.VolumeID, "uuid:"); ok {
		uuid = pin
	}
	data, err := json.MarshalIndent(map[string]string{"library_dir": dir, "volume_uuid": strings.ToUpper(uuid),
		"volume_name": name, "updated_at": time.Now().UTC().Format(time.RFC3339)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(support, 0o755); err != nil {
		return err
	}
	path := filepath.Join(support, "library.json")
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// libraryVolume is the UUID and name of the volume mounted under /Volumes
// holding dir, as the app's LibraryVolume sees it.
func libraryVolume(dir string) (uuid, name string) {
	rest, ok := strings.CutPrefix(dir, "/Volumes/")
	if !ok {
		return "", ""
	}
	output, err := exec.Command("diskutil", "info", "-plist", "/Volumes/"+strings.SplitN(rest, "/", 2)[0]).Output()
	if err != nil {
		return "", ""
	}
	parsed, err := parsePlist(output)
	info, _ := parsed.(map[string]any)
	if err != nil || info == nil {
		return "", ""
	}
	return plistString(info, "VolumeUUID"), plistString(info, "VolumeName")
}

func mcpLauncherPath(support string) string { return filepath.Join(support, "bin", "pharos-mcp") }

// mcpLauncherScript runs the MCP server from this Mac's disk: a process
// executing a binary on the library drive would keep the drive from ejecting
// and die when it is unplugged.
func mcpLauncherScript(support string) string {
	return `#!/bin/sh
# Pharos MCP server for agent clients, written by ` + "`alexandria install-mcp`" + `.
# Run it with no arguments. It runs Pharos from this Mac's disk, not from the
# library drive, so the drive can be ejected while agents stay connected. The
# Pharos app keeps these current:
#   runtime/current  a local copy of the library's Pharos.app
#   library.json     {"library_dir": "...", "volume_uuid": "..."}
support=` + shellQuote(support) + `
export PHAROS_SUPPORT_DIR="$support"
pointer="$support/library.json"
runtime="$support/runtime/current/Contents/MacOS/alexandria"
if [ -x "$runtime" ]; then
	exec "$runtime" mcp --library-json "$pointer"
fi
library=$(/usr/bin/plutil -extract library_dir raw -o - "$pointer" 2>/dev/null)
if [ -n "$library" ]; then
	for binary in "$library"/*.app/Contents/MacOS/alexandria; do
		if [ -x "$binary" ]; then
			echo "pharos-mcp: warning: there is no local Pharos runtime at $runtime, so this runs $binary from the library drive; unplugging the drive will stop this MCP server. Open Pharos on this Mac to install the runtime." >&2
			exec "$binary" mcp --library-json "$pointer"
		fi
	done
fi
echo "pharos-mcp: cannot start: there is no local Pharos runtime at $runtime and no library drive is connected (see $pointer). Open Pharos from its library drive on this Mac." >&2
exit 1
`
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

// InstallMCPLauncher writes the launcher agent clients run, unless it is
// already current, and returns its path.
func InstallMCPLauncher(support string) (string, error) {
	path, script := mcpLauncherPath(support), mcpLauncherScript(support)
	if current, err := os.ReadFile(path); err == nil && string(current) == script {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(script), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(temporary, 0o755); err != nil {
		return "", err
	}
	return path, os.Rename(temporary, path)
}
