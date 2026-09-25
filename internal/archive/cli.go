package archive

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func Run(arguments []string) error {
	configPath := ""
	filtered := []string{}
	for index := 0; index < len(arguments); index++ {
		if arguments[index] == "--config" && index+1 < len(arguments) {
			configPath = arguments[index+1]
			index++
			continue
		}
		filtered = append(filtered, arguments[index])
	}
	if len(filtered) == 0 {
		return usageError()
	}
	command := filtered[0]
	args := filtered[1:]
	if command == "init" {
		path := "archive.toml"
		if len(args) > 0 {
			path = expandPath(args[0])
		}
		if err := InitConfig(path); err != nil {
			return err
		}
		fmt.Printf("Wrote %s. Edit archive_root, pin its volume_id, and opt in source paths.\n", path)
		return nil
	}
	if command == "init-library" {
		if slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--adopt") }) {
			return runAdoptCLI(args)
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: alexandria init-library DIR [--adopt CONFIG [--full-check]]")
		}
		config, err := InitLibrary(args[0], volumeIdentity)
		if err != nil {
			return err
		}
		fmt.Printf("Wrote %s and created its catalog at %s.\n", config.Path, config.CatalogPath)
		if config.VolumeID != "" {
			fmt.Printf("The library is pinned to volume %s.\n", config.VolumeID)
		}
		return nil
	}
	if command == "volume-id" {
		if len(args) != 1 {
			return fmt.Errorf("usage: alexandria volume-id PATH")
		}
		fmt.Println(volumeIdentity(expandPath(args[0])))
		return nil
	}
	if command == "config-executable" {
		if len(args) != 2 {
			return fmt.Errorf("usage: alexandria config-executable CONFIG EXECUTABLE")
		}
		return SetRootString(expandPath(args[0]), "executable", expandPath(args[1]))
	}
	if command == "install-mcp" {
		path, err := InstallMCPLauncher(pharosSupportDir())
		if err != nil {
			return err
		}
		fmt.Printf("Wrote %s. Agent clients run it as a stdio MCP server with no arguments.\n", path)
		return nil
	}
	if command == "mcp" {
		// An agent keeps this process for its whole session, so it starts, and
		// keeps answering, while the library is unavailable. It loads the
		// configuration and runs the library guards for each request instead.
		flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
		libraryJSON := flags.String("library-json", "", "")
		if err := flags.Parse(args); err != nil {
			return err
		}
		return RunMCP(configPath, *libraryJSON, os.Stdin, os.Stdout)
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	if command == "doctor" {
		return runDoctor(config)
	}
	if err := config.checkLibrary(volumeIdentity); err != nil {
		return err
	}
	// Capturing and backing up only copy files; they never open the catalog
	// for writing.
	if command == "capture" {
		return runCaptureCLI(config, args)
	}
	if command == "backup" {
		return runBackupCLI(config, args)
	}
	if command == "add-this-mac" {
		return runAddThisMacCLI(config, args, os.Stdin, os.Stdout)
	}
	if err := config.EnsureDirs(); err != nil {
		return err
	}
	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		return err
	}
	defer catalog.Close()
	switch command {
	case "serve":
		openBrowser := false
		for _, arg := range args {
			if arg == "--open" {
				openBrowser = true
			}
		}
		if openBrowser {
			display := config.Host
			if strings.Contains(display, ":") {
				display = "[" + display + "]"
			}
			target := fmt.Sprintf("http://%s:%d/?token=%s", display, config.Port, urlQueryEscape(config.APIToken))
			go func() { _ = exec.Command("open", target).Run() }()
		}
		support := pharosSupportDir()
		if config.Library {
			if _, err := InstallMCPLauncher(support); err != nil {
				fmt.Fprintf(os.Stderr, "MCP launcher: %v\n", err)
			}
		}
		server := NewServer(config, catalog)
		// Only a service that got its port serves the library: then the MCP
		// launcher may point at it, and a release (see releasedMarkerName) ends.
		server.life.listening = func() {
			if config.Library {
				if err := recordLibrary(support, config, libraryVolume); err != nil {
					fmt.Fprintf(os.Stderr, "library.json: %v\n", err)
				}
			}
			_ = os.Remove(releasedMarker(config.CatalogPath))
		}
		return server.Serve()
	case "ingest":
		results := []IngestResult{}
		for _, source := range config.Sources {
			if !source.Enabled {
				continue
			}
			adapter, err := MakeAdapter(source)
			if err != nil {
				return err
			}
			results = append(results, catalog.Ingest(adapter, nil))
		}
		links, err := catalog.ReconcileIdentities()
		if err != nil {
			return err
		}
		if err := catalog.refreshMainIntegrations(context.Background()); err != nil {
			return err
		}
		if err := catalog.refreshAllLibrary(context.Background()); err != nil {
			return err
		}
		return printJSON(map[string]any{"sources": results, "identity_links_added": links})
	case "repair-existing":
		if len(args) != 1 {
			return fmt.Errorf("usage: alexandria repair-existing SOURCE")
		}
		var source *SourceConfig
		for index := range config.Sources {
			if config.Sources[index].Name == args[0] {
				source = &config.Sources[index]
				break
			}
		}
		if source == nil {
			return fmt.Errorf("unknown source: %s", args[0])
		}
		adapter, err := MakeAdapter(*source)
		if err != nil {
			return err
		}
		result := catalog.IngestExisting(adapter, nil)
		links, linkErr := catalog.ReconcileIdentities()
		if linkErr != nil {
			return linkErr
		}
		if result.Error != nil {
			return fmt.Errorf("repair failed: %v", result.Error)
		}
		if err := catalog.refreshAllLibrary(context.Background()); err != nil {
			return err
		}
		return printJSON(map[string]any{"source": result, "identity_links_added": links})
	case "build-tools":
		built, err := catalog.BackfillToolLedger(context.Background(), func(done, total int) {
			if done%500 == 0 || done == total {
				fmt.Fprintf(os.Stderr, "built tool ledger for %d/%d conversations\n", done, total)
			}
		})
		if err != nil {
			return fmt.Errorf("build tool ledger after %d conversations: %w", built, err)
		}
		if err := catalog.ensureToolRollup(context.Background()); err != nil {
			return err
		}
		status, err := catalog.ToolLedgerStatus(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"conversations_built": built, "status": status})
	case "refine-usage":
		refined, err := catalog.RefineUsageTimeline(func(done, total int) {
			if done%100 == 0 || done == total {
				fmt.Fprintf(os.Stderr, "refined %d/%d conversations\n", done, total)
			}
		})
		if err != nil {
			return fmt.Errorf("refine usage after %d conversations: %w", refined, err)
		}
		return printJSON(map[string]any{"conversations_refined": refined})
	case "pricing":
		return runPricingCLI(catalog, args)
	case "search":
		return runSearchCLI(catalog, args)
	case "tl1":
		return runTL1CLI(catalog, args)
	case "health":
		value := catalog.Health()
		value["storage"] = storage(config)
		value["drive"] = driveReport(config, collectDrive(context.Background(), config))
		return printJSON(value)
	case "probe":
		return runProbeCLI(config, catalog, args)
	case "index":
		return runIndexCLI(config, catalog, args)
	case "upcoming", "protect", "preserve", "reclaim", "reconcile", "tick", "worker":
		return fmt.Errorf("%s is part of mothballed TL1 reclamation; see docs/reclamation/README.md", command)
	case "enrich-github":
		return fmt.Errorf("%s is not enabled in the Go service", command)
	default:
		return usageError()
	}
}

func usageError() error {
	return fmt.Errorf("usage: alexandria [--config PATH] {init,init-library,add-this-mac,serve,capture,index,backup,ingest,repair-existing,refine-usage,build-tools,pricing,search,tl1,health,doctor,mcp,install-mcp,probe,volume-id}")
}
func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer("%", "%25", " ", "%20", "+", "%2B", "?", "%3F", "&", "%26", "=", "%3D")
	return replacer.Replace(value)
}
func printJSON(value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(payload))
	return nil
}

func runSearchCLI(catalog *Catalog, args []string) error {
	query := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query = args[0]
		parseArgs = args[1:]
	}
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	repository := flags.String("repository", "", "")
	source := flags.String("source", "", "")
	file := flags.String("file", "", "")
	owner := flags.String("owner", "", "")
	provider := flags.String("provider", "", "")
	model := flags.String("model", "", "")
	from := flags.String("from", "", "")
	to := flags.String("to", "", "")
	flavor := flags.String("flavor", "", "")
	version := flags.String("version", "", "")
	outcome := flags.String("outcome", "", "")
	errorType := flags.String("error", "", "")
	metric := flags.String("metric", "", "")
	limit := flags.Int("limit", 20, "")
	prText := flags.String("pr", "", "")
	minimumText := flags.String("minimum", "", "")
	maximumText := flags.String("maximum", "", "")
	if err := flags.Parse(parseArgs); err != nil {
		return err
	}
	if query == "" && flags.NArg() > 0 {
		query = flags.Arg(0)
	}
	options := SearchOptions{Query: query, Repository: *repository, Source: *source, File: *file, Owner: *owner, Provider: *provider, Model: *model, From: *from, To: *to, Flavor: *flavor, Version: *version, Outcome: *outcome, Error: *errorType, Metric: *metric, Limit: *limit}
	if *prText != "" {
		value, err := strconv.Atoi(*prText)
		if err != nil {
			return err
		}
		options.PR = &value
	}
	if *minimumText != "" {
		value, err := strconv.ParseFloat(*minimumText, 64)
		if err != nil {
			return err
		}
		options.Minimum = &value
	}
	if *maximumText != "" {
		value, err := strconv.ParseFloat(*maximumText, 64)
		if err != nil {
			return err
		}
		options.Maximum = &value
	}
	value, err := catalog.Search(options)
	if err != nil {
		return err
	}
	return printJSON(value)
}

func executablePath() string { path, _ := os.Executable(); path, _ = filepath.Abs(path); return path }

// runPricingCLI validates a pricing file (the embedded one by default) and
// reports which models in this catalog still lack a confirmed price, or prints
// the prompt that asks an agent to refresh the price history.
func runPricingCLI(catalog *Catalog, args []string) error {
	if len(args) == 1 && args[0] == "prompt" {
		fmt.Print(catalog.pricingStatus()["prompt"])
		return nil
	}
	if len(args) == 0 || args[0] != "check" || len(args) > 2 {
		return fmt.Errorf("usage: alexandria pricing {check [FILE],prompt}")
	}
	data, err := pricingDocumentBytes()
	if len(args) == 2 {
		data, err = os.ReadFile(expandPath(args[1]))
	}
	if err != nil {
		return err
	}
	doc, problems := parsePricing(data)
	statuses := map[string]int{}
	for _, change := range doc.Changes {
		statuses[change.Status]++
	}
	health := catalog.pricingHealth()
	if err := printJSON(map[string]any{"valid": len(problems) == 0, "problems": problems, "changes_by_status": statuses,
		"aliases": len(doc.Aliases), "catalog": health}); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("pricing file has %d problems", len(problems))
	}
	return nil
}
