package archive

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tl1Window selects which tasks a TL1 analysis covers. Since and Until bound
// task creation time; Since may be "latest" for the start of the newest large
// enqueue. Days is the older rolling window and applies only without Since.
// Scope "current" keeps only tasks run with their flavor's current definition.
type tl1Window struct {
	Days         int
	Since, Until string
	Scope        string
}

const tl1LatestEnqueue = "latest"

// tl1EnqueueListLimit is how many recent enqueues the overview lists.
const tl1EnqueueListLimit = 12

// The same thresholds as TL1's recent_large_task_enqueue_spikes, so "Latest
// enqueue" here and in TL1's dashboard start at the same moment.
const (
	tl1EnqueueMinTasks = 5
	tl1EnqueueIdleGap  = 10 * time.Minute
)

// tl1Enqueue is one large enqueue: a run of root-task creations with no gap
// longer than ten minutes, containing at least five root tasks.
type tl1Enqueue struct {
	StartedAt, EndedAt string
	RootTasks          int
	Flavors            tl1Counter
	// Changed lists flavors whose first run in this enqueue's period (until
	// the next enqueue) used a definition (shape version) no earlier task
	// used: the revision took effect with this enqueue. ChangedDuring lists
	// flavors whose definition changed partway through the period, so the
	// period mixes revisions.
	Changed, ChangedDuring []string
}

func tl1WindowFromQuery(query url.Values) tl1Window {
	days, _ := strconv.Atoi(query.Get("days"))
	return tl1Window{Days: days, Since: strings.TrimSpace(query.Get("since")), Until: strings.TrimSpace(query.Get("until")), Scope: defaultString(query.Get("scope"), "all")}
}

func tl1WindowFromArgs(args map[string]any) tl1Window {
	return tl1Window{Days: int(integer(args["days"])), Since: strings.TrimSpace(firstString(args["since"])), Until: strings.TrimSpace(firstString(args["until"])), Scope: defaultString(args["scope"], "all")}
}

// tl1WindowBound normalizes a user-supplied bound to the catalog's canonical
// time layout so it compares correctly with stored created_at values.
func tl1WindowBound(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	canonical := canonicalTime(value)
	if canonical == "" {
		return "", fmt.Errorf("invalid TL1 time bound %q; use an ISO 8601 time or %q", value, tl1LatestEnqueue)
	}
	return canonical, nil
}

// tl1EnqueueSpikes mirrors TL1's enqueue segmentation. Only root tasks count
// (no parent and not created by another task): their creation is a person or
// script enqueueing work, while child tasks are workflow progress. Results are
// newest first.
func tl1EnqueueSpikes(tasks []*tl1Task) []tl1Enqueue {
	roots := []*tl1Task{}
	for _, task := range tasks {
		if task.ParentID == "" && task.CreatedBy == "" && task.CreatedAt != "" {
			roots = append(roots, task)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].CreatedAt < roots[j].CreatedAt })
	spikes := []tl1Enqueue{}
	var current *tl1Enqueue
	var previous time.Time
	finish := func() {
		if current != nil && current.RootTasks >= tl1EnqueueMinTasks {
			spikes = append(spikes, *current)
		}
	}
	for _, task := range roots {
		created, ok := parseTime(task.CreatedAt)
		if !ok {
			// A malformed legacy timestamp must not break the analysis.
			continue
		}
		if current == nil || created.Sub(previous) > tl1EnqueueIdleGap {
			finish()
			current = &tl1Enqueue{StartedAt: task.CreatedAt, Flavors: tl1Counter{}}
		}
		current.RootTasks++
		current.EndedAt = task.CreatedAt
		current.Flavors[task.Flavor]++
		previous = created
	}
	finish()
	tl1MarkDefinitionChanges(spikes, tasks)
	for left, right := 0, len(spikes)-1; left < right; left, right = left+1, right-1 {
		spikes[left], spikes[right] = spikes[right], spikes[left]
	}
	return spikes
}

// tl1MarkDefinitionChanges fills each enqueue's Changed and ChangedDuring
// (spikes oldest first). A flavor's first definition is not a change.
func tl1MarkDefinitionChanges(spikes []tl1Enqueue, tasks []*tl1Task) {
	if len(spikes) == 0 {
		return
	}
	ordered := append([]*tl1Task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt < ordered[j].CreatedAt })
	seen := map[string]bool{}
	versions := map[string]int{}
	ranInPeriod, atStart, during := map[string]bool{}, map[string]bool{}, map[string]bool{}
	index := -1
	flush := func() {
		if index >= 0 {
			spikes[index].Changed, spikes[index].ChangedDuring = tl1SortedKeys(atStart), tl1SortedKeys(during)
		}
		ranInPeriod, atStart, during = map[string]bool{}, map[string]bool{}, map[string]bool{}
	}
	for _, task := range ordered {
		for index+1 < len(spikes) && task.CreatedAt >= spikes[index+1].StartedAt {
			flush()
			index++
		}
		if task.ShapeVersion == "" {
			continue
		}
		key := task.Flavor + "\x1f" + task.ShapeVersion
		if !seen[key] {
			seen[key] = true
			versions[task.Flavor]++
			switch {
			case index < 0 || versions[task.Flavor] == 1:
			case ranInPeriod[task.Flavor]:
				during[task.Flavor] = true
			default:
				atStart[task.Flavor] = true
			}
		}
		ranInPeriod[task.Flavor] = true
	}
	flush()
}

func tl1SortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// tl1ResolveWindow resolves the window's bounds against the installation's
// enqueues (newest first).
func tl1ResolveWindow(window tl1Window, spikes []tl1Enqueue, now time.Time) (since, until string, err error) {
	switch {
	case window.Since == tl1LatestEnqueue:
		if len(spikes) > 0 {
			since = spikes[0].StartedAt
		}
	case window.Since != "":
		if since, err = tl1WindowBound(window.Since); err != nil {
			return "", "", err
		}
	case window.Days > 0:
		since = formatTime(now.AddDate(0, 0, -window.Days))
	}
	if until, err = tl1WindowBound(window.Until); err != nil {
		return "", "", err
	}
	return since, until, nil
}

func (spike tl1Enqueue) row(tasksSince int) map[string]any {
	return map[string]any{"started_at": spike.StartedAt, "ended_at": spike.EndedAt, "root_task_count": spike.RootTasks,
		"flavors": spike.Flavors.top(4), "changed_flavors": spike.Changed,
		"changed_during": spike.ChangedDuring, "tasks_since": tasksSince}
}

// tl1EnqueueRows returns the newest enqueues for the UI and agents, each with
// how many tasks were created from its start onward.
func tl1EnqueueRows(data *tl1Data, limit int) []map[string]any {
	rows := []map[string]any{}
	for index, spike := range data.Enqueues {
		if index >= limit {
			break
		}
		count := 0
		for _, task := range data.AllTasks {
			if task.CreatedAt >= spike.StartedAt {
				count++
			}
		}
		rows = append(rows, spike.row(count))
	}
	return rows
}

func (window tl1Window) mode() string {
	switch {
	case window.Since == tl1LatestEnqueue:
		return "latest_enqueue"
	case window.Since != "" || window.Until != "":
		return "custom"
	case window.Days > 0:
		return "days"
	}
	return "all"
}

// tl1WindowRow describes the resolved window for the UI and agents.
func tl1WindowRow(data *tl1Data) map[string]any {
	row := map[string]any{"mode": data.Window.mode(), "since": nilIfEmpty(data.Since), "until": nilIfEmpty(data.Until),
		"days": data.Window.Days, "label": tl1WindowLabel(data), "tasks": len(data.Tasks), "total_tasks": len(data.AllTasks),
		"mixed_definitions": tl1MixedDefinitions(data)}
	if data.Window.mode() == "latest_enqueue" && len(data.Enqueues) == 0 {
		row["note"] = "No large enqueue (5+ root tasks within 10 minutes) was found, so all tasks are shown."
	}
	return row
}

func tl1WindowLabel(data *tl1Data) string {
	switch {
	case data.Window.mode() == "latest_enqueue" && len(data.Enqueues) > 0:
		return fmt.Sprintf("tasks created since the latest large enqueue (%s, %d root tasks)", tl1Day(data.Since), data.Enqueues[0].RootTasks)
	case data.Since != "" && data.Until != "":
		return "tasks created from " + tl1Day(data.Since) + " until " + tl1Day(data.Until)
	case data.Since != "":
		return "tasks created since " + tl1Day(data.Since)
	case data.Until != "":
		return "tasks created before " + tl1Day(data.Until)
	}
	return "all tasks"
}

func tl1UntilArg(data *tl1Data) string {
	if data.Until == "" {
		return ""
	}
	return " until=" + data.Until
}

// tl1MixedDefinitions lists flavors whose tasks in the window ran more than
// one definition (shape version), so their metrics blend revisions.
func tl1MixedDefinitions(data *tl1Data) []string {
	versions := map[string]map[string]bool{}
	for _, task := range data.Tasks {
		if task.ShapeVersion == "" {
			continue
		}
		if versions[task.Flavor] == nil {
			versions[task.Flavor] = map[string]bool{}
		}
		versions[task.Flavor][task.ShapeVersion] = true
	}
	mixed := map[string]bool{}
	for flavor, seen := range versions {
		if len(seen) > 1 {
			mixed[flavor] = true
		}
	}
	return tl1SortedKeys(mixed)
}
