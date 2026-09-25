package archive

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// add-this-mac is the one step for a Mac plugged into a library: find its
// conversation sources, add them to this Mac's host file, capture them onto
// the library's drive, index them, and install the MCP launcher. Rerunning it
// picks up new sources and captures and indexes only what changed.
func runAddThisMacCLI(config Config, args []string, in io.Reader, out io.Writer) error {
	flags := flag.NewFlagSet("add-this-mac", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "add every source found without asking")
	captureOnly := flags.Bool("capture-only", false, "capture onto the drive and stop; index later, on any Mac")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !config.Library {
		return errNotLibrary(config)
	}
	host := currentHost()
	if host.Fallback {
		return errors.New("Pharos could not identify this Mac (ioreg did not answer); set PHAROS_HOST_ID to a stable name and run this again")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	library := filepath.Dir(config.Path)
	drive := volumeName(library)
	fmt.Fprintf(out, "Adding %s to the Pharos library at %s.\n\n", host.Label, library)

	catalog, err := OpenCatalog(config.CatalogPath)
	if err != nil {
		return err
	}
	defer catalog.Close()

	report := ProbeSources(config, catalog)
	fmt.Fprintln(out, "Found on this Mac:")
	adding := 0
	for _, candidate := range report.Candidates {
		if !candidate.Acceptable {
			continue
		}
		state := "new"
		if candidate.Configured != nil {
			state = "already added"
		} else if candidate.Recommended {
			adding++
		} else {
			state = "not recommended; add it in the app if you want it"
		}
		where := candidate.DisplayPath
		if where == "" {
			where = candidate.Path
		}
		fmt.Fprintf(out, "  %-10s %-48s %s, %s (%s)\n", candidate.Name, where, plural(candidate.Count, defaultString(candidate.Unit, "session")), humanBytes(candidate.Bytes), state)
		if candidate.Retention != "" {
			fmt.Fprintf(out, "             %s\n", candidate.Retention)
		}
	}
	if adding == 0 && len(config.Sources) == 0 {
		return errors.New("no conversation sources were found on this Mac")
	}
	if adding > 0 {
		if !*yes && !confirm(in, out, fmt.Sprintf("\nAdd %s to the library? [Y/n] ", plural(adding, "new source"))) {
			fmt.Fprintln(out, "Nothing was changed.")
			return nil
		}
		result, err := AcceptProbe(config, report, ProbeSelection{All: true})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Saved this Mac's sources in %s.\n", result.HostFile)
		if config, err = LoadConfig(config.Path); err != nil {
			return err
		}
	}

	sources, err := captureSources(config, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nCapturing %s onto %s…\n", plural(len(sources), "source"), drive)
	started := time.Now()
	summary, err := Capture(ctx, config, sources, func(result CaptureSourceResult) {
		if result.State != "running" {
			fmt.Fprintf(out, "  %-10s %s: %d files and %d database snapshots, %s copied\n", result.Name, result.State,
				result.FilesCopied, result.SnapshotsTaken, humanBytes(result.BytesCopied+result.SnapshotBytes))
			for _, failure := range result.Errors {
				fmt.Fprintf(out, "             error: %s %s\n", failure.Path, failure.Error)
			}
		}
	})
	if err != nil {
		return err
	}
	if !summary.OK {
		for _, failure := range summary.Errors {
			fmt.Fprintf(out, "  error: %s %s\n", failure.Path, failure.Error)
		}
		return errors.New("the capture finished with errors; run this again to retry")
	}
	fmt.Fprintf(out, "Captured in %s. You can eject %s now: indexing can run later on any Mac with this drive.\n", time.Since(started).Round(time.Second), drive)
	if *captureOnly {
		fmt.Fprintln(out, "To index later, open Pharos from the drive, or run this again.")
		return nil
	}

	targets, err := captureTargets(config.CaptureRoot, []string{host.ID}, false, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nIndexing %s. The first index of a large history can take an hour or more; Ctrl-C stops safely, and running this again resumes.\n", plural(len(targets), "source"))
	results, _, err := catalog.indexCaptureTargets(ctx, targets, func(target captureTarget, result IngestResult, elapsed time.Duration) {
		status := "done"
		if result.Error != nil {
			status = fmt.Sprint("error: ", result.Error)
		}
		fmt.Fprintf(out, "  %-10s %s written, %d parts parsed, %d unchanged, in %s (%s)\n", target.Source,
			plural(result.Workspaces, "workspace"), result.Parsed, result.Unchanged, elapsed.Round(time.Second), status)
	})
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("stopped; run this again to resume indexing")
	}
	failed := 0
	for _, result := range results {
		if result.Error != nil {
			failed++
		}
	}

	if launcher, err := InstallMCPLauncher(pharosSupportDir()); err != nil {
		fmt.Fprintf(out, "\nCould not install the MCP launcher: %v\n", err)
	} else {
		fmt.Fprintf(out, "\nAgent clients on this Mac can use Pharos as a stdio MCP server: %s\n", launcher)
	}
	if failed > 0 {
		return fmt.Errorf("%s did not index; run this again to retry", plural(failed, "source"))
	}
	fmt.Fprintf(out, "Done. Open %s to browse the library.\n", filepath.Join(library, "Pharos.app"))
	return nil
}

// confirm asks a yes/no question, defaulting to yes. With no answer to read
// (input closed, as when run unattended) it declines: adding sources needs a
// person or --yes.
func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprint(out, question)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && answer == "" {
		fmt.Fprintln(out, "\nNo answer; run with --yes to add them without asking.")
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes"
}
