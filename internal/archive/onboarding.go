package archive

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

// ProbeSelection chooses probe candidates by their suggested names. Accepted
// ones are written enabled; declined ones are written disabled, so the choice
// is remembered and can be reversed from Sources.
type ProbeSelection struct {
	Accept  []string          `json:"accept"`
	Decline []string          `json:"decline"`
	Names   map[string]string `json:"names"`
	All     bool              `json:"all"`
}

type ProbeAcceptResult struct {
	HostFile string   `json:"host_file"`
	Added    []string `json:"added"`
	Enabled  []string `json:"enabled"`
	Declined []string `json:"declined"`
	Skipped  []string `json:"skipped"`
}

// probeRequestError is a problem with the request rather than the library.
type probeRequestError struct{ error }

func errNotLibrary(config Config) error {
	return probeRequestError{fmt.Errorf("%s is not a portable library (library = true), so probe results cannot be saved per Mac; add the [[sources]] you want to it by hand", config.Path)}
}

// AcceptProbe writes the selected candidates to this host's source file.
// Everything is validated before the file is written.
func AcceptProbe(config Config, report ProbeReport, selection ProbeSelection) (ProbeAcceptResult, error) {
	path := hostConfigPath(config)
	result := ProbeAcceptResult{HostFile: path, Added: []string{}, Enabled: []string{}, Declined: []string{}, Skipped: []string{}}
	if currentHost().Fallback {
		return result, errHostUnknown
	}
	if path == "" {
		return result, errNotLibrary(config)
	}
	candidates := map[string]ProbeCandidate{}
	accept := append([]string{}, selection.Accept...)
	for _, candidate := range report.Candidates {
		candidates[candidate.Name] = candidate
		if selection.All && candidate.Recommended {
			accept = append(accept, candidate.Name)
		}
	}
	hostFileMu.Lock()
	defer hostFileMu.Unlock()
	// A concurrent accept may have written names this config has not loaded.
	written, _ := readSourceBlocks(path, "")
	taken := map[string]bool{}
	for _, sources := range [][]SourceConfig{config.Sources, written} {
		for _, source := range sources {
			taken[source.Name] = true
		}
	}
	decided := map[string]bool{}
	additions := []SourceConfig{}
	enable := []string{}
	choose := func(name string, enabled bool) error {
		name = strings.TrimSpace(name)
		if name == "" || decided[name] {
			return nil
		}
		decided[name] = true
		candidate, ok := candidates[name]
		if !ok {
			return probeRequestError{fmt.Errorf("no candidate source named %q was found on this Mac; run the probe again", name)}
		}
		if !candidate.Acceptable {
			return probeRequestError{fmt.Errorf("%s cannot be added automatically: %s", name, candidate.Detail)}
		}
		if configured := candidate.Configured; configured != nil {
			switch {
			case !enabled || configured.Enabled:
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s is already configured as %s", candidate.DisplayPath, configured.Name))
			case configured.Scope == "host":
				enable = append(enable, configured.Name)
			default:
				result.Skipped = append(result.Skipped, fmt.Sprintf("%s is configured for every Mac in %s; enable it there", configured.Name, configured.File))
			}
			return nil
		}
		final := name
		if custom := strings.TrimSpace(selection.Names[name]); custom != "" {
			final = custom
		}
		if !sourceNameValid.MatchString(final) {
			return probeRequestError{fmt.Errorf("source name %q must start with a letter or digit and use only letters, digits, '.', '_' and '-'", final)}
		}
		if taken[final] {
			return probeRequestError{fmt.Errorf("a source named %q already exists", final)}
		}
		taken[final] = true
		additions = append(additions, SourceConfig{Name: final, Kind: candidate.Kind, Path: candidate.Path, Enabled: enabled})
		return nil
	}
	for _, name := range accept {
		if err := choose(name, true); err != nil {
			return result, err
		}
	}
	for _, name := range selection.Decline {
		if err := choose(name, false); err != nil {
			return result, err
		}
	}
	if err := appendHostSources(path, report.Host, additions); err != nil {
		return result, err
	}
	for _, source := range additions {
		if source.Enabled {
			result.Added = append(result.Added, source.Name)
		} else {
			result.Declined = append(result.Declined, source.Name)
		}
	}
	for _, name := range enable {
		if err := SetSourceEnabled(path, name, true); err != nil {
			return result, err
		}
		result.Enabled = append(result.Enabled, name)
	}
	return result, nil
}

func (s *Server) acceptProbe(w http.ResponseWriter, body map[string]any) {
	config := s.Config()
	if !config.Library {
		writeError(w, errNotLibrary(config), http.StatusBadRequest)
		return
	}
	var selection ProbeSelection
	payload, _ := json.Marshal(body)
	if err := json.Unmarshal(payload, &selection); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	result, err := AcceptProbe(config, ProbeSources(config, s.Catalog), selection)
	if err == nil {
		err = s.reloadSources()
	}
	var request probeRequestError
	switch {
	case errors.As(err, &request):
		writeError(w, err, http.StatusBadRequest)
	case err != nil:
		writeError(w, err, http.StatusInternalServerError)
	default:
		writeJSON(w, result, http.StatusOK)
	}
}

// reloadSources re-reads source definitions only; the rest of the running
// configuration, including a generated API token, stays as it is.
func (s *Server) reloadSources() error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	reloaded, err := LoadConfig(s.config.Path)
	if err != nil {
		return err
	}
	s.config.Sources = reloaded.Sources
	return nil
}

func runProbeCLI(config Config, catalog *Catalog, args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	accept := flags.String("accept", "", "comma-separated candidate names to add; NAME=CUSTOM renames one")
	all := flags.Bool("accept-all", false, "add every recommended candidate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report := ProbeSources(config, catalog)
	if *accept == "" && !*all {
		return printJSON(report)
	}
	if !config.Library {
		return errNotLibrary(config)
	}
	selection := ProbeSelection{All: *all, Names: map[string]string{}}
	for _, item := range strings.Split(*accept, ",") {
		candidate, name, renamed := strings.Cut(strings.TrimSpace(item), "=")
		selection.Accept = append(selection.Accept, candidate)
		if renamed {
			selection.Names[candidate] = name
		}
	}
	result, err := AcceptProbe(config, report, selection)
	if err != nil {
		return err
	}
	return printJSON(result)
}
