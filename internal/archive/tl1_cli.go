package archive

import (
	"flag"
	"fmt"
)

// runTL1CLI prints the TL1 analysis: `alexandria tl1 [--project NAME] [--installation ID]
// [--since latest|TIME] [--until TIME] [--days N] [--scope current] [--prompt]`. --prompt prints only the review
// prompt, ready to paste into an agent.
func runTL1CLI(catalog *Catalog, args []string) error {
	flags := flag.NewFlagSet("tl1", flag.ContinueOnError)
	project := flags.String("project", "", "TL1 project, combined across the Macs that run it (default: most recently active)")
	installation := flags.String("installation", "", "TL1 installation ID, for one Mac's runs of a project")
	since := flags.String("since", "", "only tasks created at or after this ISO 8601 time, or \"latest\" for the latest large enqueue")
	until := flags.String("until", "", "only tasks created before this ISO 8601 time")
	days := flags.Int("days", 0, "only tasks created in the last N days (0 = all); ignored with --since")
	scope := flags.String("scope", "all", "all, or current to keep only runs of each flavor's current definition")
	prompt := flags.Bool("prompt", false, "print the optimization review prompt instead of JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	overview, err := catalog.TL1Overview(tl1Selection{Project: *project, Installation: *installation}, tl1Window{Days: *days, Since: *since, Until: *until, Scope: *scope})
	if err != nil {
		return err
	}
	if *prompt {
		fmt.Println(defaultString(overview["review_prompt"], "No TL1 installations are indexed."))
		return nil
	}
	return printJSON(overview)
}
