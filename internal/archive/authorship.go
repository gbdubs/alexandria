package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Human authorship estimates how much of the text sent as user turns a person
// typed (or dictated). A user turn often carries more than that: harness
// instructions, one-click prompts, attachment references, agent output copied
// from another chat, logs, and prompts a script sent. Each turn is split into
// spans, each labelled with a category and the reason it was assigned, so the
// totals are explainable message by message. Definitions are in
// docs/human-authorship.md.

// authorshipVersion names the classification rules; a new version rebuilds.
const authorshipVersion = "authorship-v2"

const (
	// authorshipWindow bounds how far back copied text is looked for.
	authorshipWindow = 48 * time.Hour
	// shingleWords is the length of the word runs compared between texts.
	shingleWords = 8
	// quoteMinWords is the shortest matching run treated as copied.
	quoteMinWords = 12
	// Prompts with the same text in this many sessions on this many days are
	// templates (buttons, saved prompts) rather than text typed each time.
	templateSessions, templateDays, templateMinChars = 5, 3, 10
	// typingCharsPerSecond is faster than anyone types or dictates.
	typingCharsPerSecond = 20
	// typingCheckMinChars keeps the speed check off short replies.
	typingCheckMinChars = 600
	// authorshipRebuildSpacing keeps frequent syncs from rebuilding constantly.
	authorshipRebuildSpacing = 5 * time.Minute
)

// Span categories. Typed is what remains after every other rule. Pasted is
// the one inferred category: its rules are heuristics, so the typed total is
// reported as a range from typed to typed plus pasted.
const (
	spanTyped      = "typed"
	spanHarness    = "harness"
	spanAutomated  = "automated"
	spanTemplate   = "template"
	spanAttachment = "attachment"
	spanQuoted     = "quoted"
	spanResent     = "resent"
	spanPasted     = "pasted"
)

// authorshipCategories orders the categories message_authorship stores a
// <category>_chars and <category>_words column for.
var authorshipCategories = []string{spanTyped, spanHarness, spanAutomated, spanTemplate, spanAttachment, spanQuoted, spanResent, spanPasted}

// authorshipColumns lists each category's count columns, in category order.
func authorshipColumns() []string {
	columns := []string{}
	for _, category := range authorshipCategories {
		columns = append(columns, category+"_chars", category+"_words")
	}
	return columns
}

// authorSpan is one labelled byte range of a user message.
type authorSpan struct {
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Category string `json:"category"`
	Reason   string `json:"reason,omitempty"`
	// Source and Message name the conversation and message holding the text
	// a quoted or resent span matches.
	Source  string `json:"source,omitempty"`
	Message string `json:"message,omitempty"`
}

// authorshipState tracks the in-process rebuild.
type authorshipState struct {
	mu        sync.Mutex
	running   bool
	lastError string
	builtAt   time.Time
}

var harnessLabels = map[string]string{
	"conductor_instruction":            "Conductor system instruction",
	"system_reminder":                  "System reminder",
	"environments.environment_context": "Environment context",
	"user_instructions":                "Project instructions",
	"plugins.recommendations":          "Plugin recommendations",
}

var (
	attachmentPattern = regexp.MustCompile(`@⟦[^⟧\n]*⟧\([^)\n]*\)|(?:/[^\s]*)?\.context/attachments/(?:[A-Za-z0-9_-]+/)?[^\n]*?\.(?:md|txt|png|jpe?g|gif|webp|pdf|json|csv|log|html|zip)\b|\(image attachment\)`)
	slashPattern      = regexp.MustCompile(`^/[A-Za-z][\w:-]*`)
	fencePattern      = regexp.MustCompile("(?s)```.*?(?:```|$)")
	timestampPattern  = regexp.MustCompile(`^\[?\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}|^\[?\d{2}:\d{2}:\d{2}`)
	fileLinePattern   = regexp.MustCompile(`\S+\.[A-Za-z]{1,5}:\d+`)
	levelPattern      = regexp.MustCompile(`^\[?(?:DEBUG|INFO|WARN|WARNING|ERROR|FATAL|TRACE|debug|info|warn|error)\b[\]:]?`)
)

// harnessTags wrap text a harness prepends to what a person typed, inside the
// same message. Conductor prefixes <system_instruction> blocks to a session's
// first turn and to turns that carry attachments. The message keeps this text,
// since it is part of the prompt; authorship only counts it apart.
var harnessTags = []struct{ tag, source string }{
	{"system_instruction", "conductor_instruction"},
	{"system-reminder", "system_reminder"},
	{"environment_context", "environments.environment_context"},
	{"user_instructions", "user_instructions"},
	{"recommended_plugins", "plugins.recommendations"},
}

// harnessBlock is one harness-tagged block found at the start of a message.
type harnessBlock struct {
	Start, End int
	Source     string
}

// leadingHarnessBlocks finds the harness-tagged blocks that open text. Only
// leading blocks count: the same tags later in a message are more likely text
// a person pasted or wrote about.
func leadingHarnessBlocks(text string) []harnessBlock {
	blocks := []harnessBlock{}
	offset := 0
	for {
		rest := text[offset:]
		start := offset + len(rest) - len(strings.TrimLeft(rest, " \t\r\n"))
		matched := false
		for _, known := range harnessTags {
			open, close := "<"+known.tag+">", "</"+known.tag+">"
			if !strings.HasPrefix(text[start:], open) {
				continue
			}
			end := strings.Index(text[start:], close)
			if end < 0 {
				return blocks
			}
			end += start + len(close)
			blocks = append(blocks, harnessBlock{Start: start, End: end, Source: known.source})
			offset, matched = end, true
			break
		}
		if !matched {
			return blocks
		}
	}
}

// splitHarnessText separates leading harness blocks from the text after them.
func splitHarnessText(text string) ([]harnessBlock, string) {
	blocks := leadingHarnessBlocks(text)
	if len(blocks) == 0 {
		return nil, text
	}
	return blocks, strings.TrimSpace(text[blocks[len(blocks)-1].End:])
}

// authorInput is one user message and what is known about who sent it.
type authorInput struct {
	ID, ConversationID, WorkspaceID, Text, Sender, SourceKind string
	SubAgent                                                  bool
	SentAt                                                    time.Time
}

// authorResult is the classification of one message.
type authorResult struct {
	Input authorInput
	Spans []authorSpan
}

// textOrigin names the message a span's text was found in.
type textOrigin struct {
	conversation, message string
}

// shingleRef records where and when a word run was last seen.
type shingleRef struct {
	at int64
	textOrigin
}

// shingleIndex holds the word runs of texts seen within the window.
type shingleIndex struct {
	seen  map[uint64]shingleRef
	queue []shingleBatch
}

type shingleBatch struct {
	at     int64
	hashes []uint64
}

func newShingleIndex() *shingleIndex { return &shingleIndex{seen: map[uint64]shingleRef{}} }

func (index *shingleIndex) add(at time.Time, origin textOrigin, hashes []uint64) {
	if len(hashes) == 0 {
		return
	}
	stamp := at.Unix()
	for _, hash := range hashes {
		index.seen[hash] = shingleRef{at: stamp, textOrigin: origin}
	}
	index.queue = append(index.queue, shingleBatch{at: stamp, hashes: hashes})
}

// evict forgets runs last seen before cutoff. Batches arrive in time order.
func (index *shingleIndex) evict(cutoff time.Time) {
	stamp, drop := cutoff.Unix(), 0
	for drop < len(index.queue) && index.queue[drop].at < stamp {
		for _, hash := range index.queue[drop].hashes {
			if ref, ok := index.seen[hash]; ok && ref.at < stamp {
				delete(index.seen, hash)
			}
		}
		drop++
	}
	if drop > 0 {
		index.queue = append(index.queue[:0], index.queue[drop:]...)
	}
}

// wordToken is one normalized word and its byte range in the source text.
type wordToken struct {
	start, end int
	word       string
}

// wordTokens splits text into lowercase letter-and-digit words.
func wordTokens(text string) []wordToken {
	tokens := []wordToken{}
	start := -1
	for offset, r := range text {
		inWord := unicode.IsLetter(r) || unicode.IsDigit(r)
		if inWord && start < 0 {
			start = offset
		} else if !inWord && start >= 0 {
			tokens = append(tokens, wordToken{start, offset, strings.ToLower(text[start:offset])})
			start = -1
		}
	}
	if start >= 0 {
		tokens = append(tokens, wordToken{start, len(text), strings.ToLower(text[start:])})
	}
	return tokens
}

// shingles hashes every run of shingleWords consecutive words.
func shingles(tokens []wordToken) []uint64 {
	if len(tokens) < shingleWords {
		return nil
	}
	hashes := make([]uint64, 0, len(tokens)-shingleWords+1)
	for index := 0; index+shingleWords <= len(tokens); index++ {
		hash := fnv.New64a()
		for _, token := range tokens[index : index+shingleWords] {
			hash.Write([]byte(token.word))
			hash.Write([]byte{0})
		}
		hashes = append(hashes, hash.Sum64())
	}
	return hashes
}

// copiedRun is a run of words matching text an index saw earlier.
type copiedRun struct {
	start, end int
	origin     textOrigin
	at         int64
}

// copiedRuns finds runs of at least quoteMinWords words whose every word lies
// in a shingle the index saw at or after cutoff.
func copiedRuns(tokens []wordToken, hashes []uint64, index *shingleIndex, cutoff time.Time) []copiedRun {
	covered := make([]int, len(tokens))
	refs := make([]shingleRef, len(tokens))
	stamp := cutoff.Unix()
	for position, hash := range hashes {
		ref, ok := index.seen[hash]
		if !ok || ref.at < stamp {
			continue
		}
		for offset := position; offset < position+shingleWords; offset++ {
			covered[offset]++
			if refs[offset].conversation == "" || ref.at > refs[offset].at {
				refs[offset] = ref
			}
		}
	}
	runs := []copiedRun{}
	for position := 0; position < len(tokens); {
		if covered[position] == 0 {
			position++
			continue
		}
		end := position
		for end < len(tokens) && covered[end] > 0 {
			end++
		}
		if end-position >= quoteMinWords {
			ref := refs[position]
			runs = append(runs, copiedRun{start: tokens[position].start, end: tokens[end-1].end, origin: ref.textOrigin, at: ref.at})
		}
		position = end
	}
	return runs
}

// labeler assigns each byte of a message to at most one category; the first
// rule to claim a byte keeps it.
type labeler struct {
	text    string
	owner   []int16
	details []authorSpan
}

func newLabeler(text string) *labeler {
	owner := make([]int16, len(text))
	for index := range owner {
		owner[index] = -1
	}
	return &labeler{text: text, owner: owner}
}

func (l *labeler) claim(start, end int, category, reason string, origin textOrigin) {
	start, end = max(start, 0), min(end, len(l.text))
	if start >= end {
		return
	}
	id := int16(len(l.details))
	claimed := false
	for index := start; index < end; index++ {
		if l.owner[index] < 0 {
			l.owner[index] = id
			claimed = true
		}
	}
	if claimed {
		l.details = append(l.details, authorSpan{Category: category, Reason: reason, Source: origin.conversation, Message: origin.message})
	}
}

// unclaimed returns the length of text no rule has claimed.
func (l *labeler) unclaimed() int {
	count := 0
	for _, owner := range l.owner {
		if owner < 0 {
			count++
		}
	}
	return count
}

// spans merges labelled bytes into ordered spans. Unclaimed bytes are typed,
// except whitespace-only gaps, which join the span before them.
func (l *labeler) spans() []authorSpan {
	result := []authorSpan{}
	for index := 0; index < len(l.owner); {
		end := index
		for end < len(l.owner) && l.owner[end] == l.owner[index] {
			end++
		}
		span := authorSpan{Start: index, End: end, Category: spanTyped}
		if owner := l.owner[index]; owner >= 0 {
			detail := l.details[owner]
			span.Category, span.Reason, span.Source, span.Message = detail.Category, detail.Reason, detail.Source, detail.Message
		} else if strings.TrimSpace(l.text[index:end]) == "" && len(result) > 0 {
			result[len(result)-1].End = end
			index = end
			continue
		}
		if len(result) > 0 && result[len(result)-1].Category == span.Category && result[len(result)-1].Reason == span.Reason && result[len(result)-1].Message == span.Message {
			result[len(result)-1].End = end
		} else {
			result = append(result, span)
		}
		index = end
	}
	return result
}

// templateKey normalizes a message for template counting: attachment
// references become a placeholder and whitespace and case are ignored.
func templateKey(text string) string {
	_, text = splitHarnessText(text)
	text = attachmentPattern.ReplaceAllString(text, "@")
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// templateUse counts the sessions and days a template key was sent in.
type templateUse struct {
	sessions, days map[string]bool
}

func (use templateUse) template() bool {
	return len(use.sessions) >= templateSessions && len(use.days) >= templateDays
}

// automatedReason explains why a message came from a program or agent, or
// returns "" when a person sent it.
func automatedReason(input authorInput) string {
	switch {
	case input.Sender == "automation:claude-sdk-cli":
		return "Sent by a headless claude -p run"
	case input.Sender == "automation:codex-exec":
		return "Sent by a codex exec run"
	case strings.HasPrefix(input.Sender, "automation:"):
		return "Sent by automation " + strings.TrimPrefix(input.Sender, "automation:")
	case strings.HasPrefix(input.Sender, "agent:"):
		return "Sent by another agent session"
	case input.SourceKind == "tl1" || input.SourceKind == "tl1-export":
		return "Sent by TL1 orchestration"
	case input.SubAgent:
		// Forked sub-agents also carry the parent's history, already counted there.
		return "Sub-agent conversation"
	}
	return ""
}

// classifier holds the corpus state classification needs: templates, and
// the agent output and user text of the window before the current message.
type classifier struct {
	templates    map[string]*templateUse
	agentText    *shingleIndex
	userText     *shingleIndex
	lastUserSent map[string]time.Time
}

func newClassifier(inputs []authorInput) *classifier {
	templates := map[string]*templateUse{}
	for _, input := range inputs {
		if automatedReason(input) != "" {
			continue
		}
		key := templateKey(input.Text)
		if len(key) < templateMinChars {
			continue
		}
		use := templates[key]
		if use == nil {
			use = &templateUse{sessions: map[string]bool{}, days: map[string]bool{}}
			templates[key] = use
		}
		use.sessions[input.ConversationID] = true
		use.days[input.SentAt.Local().Format("2006-01-02")] = true
	}
	return &classifier{templates: templates, agentText: newShingleIndex(), userText: newShingleIndex(), lastUserSent: map[string]time.Time{}}
}

// classify labels one message. Agent output sent before it must already be
// in agentText; classify adds the message's own text to userText.
func (c *classifier) classify(input authorInput) []authorSpan {
	text := input.Text
	l := newLabeler(text)
	previous, hasPrevious := c.lastUserSent[input.ConversationID]
	c.lastUserSent[input.ConversationID] = input.SentAt
	if reason := automatedReason(input); reason != "" {
		l.claim(0, len(text), spanAutomated, reason, textOrigin{})
		return l.spans()
	}
	bodyStart := 0
	for _, block := range leadingHarnessBlocks(text) {
		l.claim(block.Start, block.End, spanHarness, defaultString(harnessLabels[block.Source], "Harness context"), textOrigin{})
		bodyStart = block.End
	}
	bodyStart += len(text[bodyStart:]) - len(strings.TrimLeft(text[bodyStart:], " \t\r\n"))
	body := strings.TrimSpace(text[bodyStart:])
	if use := c.templates[templateKey(text)]; use != nil && use.template() {
		l.claim(0, len(text), spanTemplate, fmt.Sprintf("Repeated prompt: sent in %d sessions on %d days", len(use.sessions), len(use.days)), textOrigin{})
	}
	if match := slashPattern.FindStringIndex(body); match != nil && !strings.Contains(body, "\n") {
		l.claim(bodyStart+match[0], bodyStart+match[1], spanTemplate, "Slash command", textOrigin{})
	}
	for _, match := range attachmentPattern.FindAllStringIndex(text, -1) {
		l.claim(match[0], match[1], spanAttachment, "Attachment reference", textOrigin{})
	}
	if strings.HasPrefix(strings.TrimSpace(body), "## Page Feedback:") {
		l.claim(bodyStart, len(text), spanPasted, "Generated page feedback", textOrigin{})
	}
	for _, match := range fencePattern.FindAllStringIndex(text, -1) {
		l.claim(match[0], match[1], spanPasted, "Code block", textOrigin{})
	}
	lines := textLines(text)
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(text[line[0]:line[1]], " "), ">") {
			l.claim(line[0], line[1], spanQuoted, "Quoted with >", textOrigin{})
		}
	}
	tokens := wordTokens(text)
	hashes := shingles(tokens)
	cutoff := input.SentAt.Add(-authorshipWindow)
	for _, run := range copiedRuns(tokens, hashes, c.agentText, cutoff) {
		l.claim(run.start, run.end, spanQuoted, "Matches agent output from "+ago(input.SentAt, run.at)+" earlier", run.origin)
	}
	for _, run := range copiedRuns(tokens, hashes, c.userText, cutoff) {
		l.claim(run.start, run.end, spanResent, "Already sent "+ago(input.SentAt, run.at)+" earlier", run.origin)
	}
	claimStructuredBlocks(l, text, lines)
	if remaining := l.unclaimed(); hasPrevious && remaining >= typingCheckMinChars {
		seconds := input.SentAt.Sub(previous).Seconds()
		if seconds <= 0 || float64(remaining)/seconds > typingCharsPerSecond {
			l.claim(0, len(text), spanPasted, fmt.Sprintf("Sent %s after the previous message, faster than typing", roundDuration(input.SentAt.Sub(previous))), textOrigin{})
		}
	}
	c.userText.add(input.SentAt, textOrigin{input.ConversationID, input.ID}, hashes)
	return l.spans()
}

// textLines returns the byte range of every line, without its newline.
func textLines(text string) [][2]int {
	lines := [][2]int{}
	start := 0
	for index := 0; index <= len(text); index++ {
		if index == len(text) || text[index] == '\n' {
			lines = append(lines, [2]int{start, index})
			start = index + 1
		}
	}
	return lines
}

// machineLine reports whether a line looks like program output or data
// rather than prose: log lines, stack frames, diffs, JSON, and file:line lists.
func machineLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	switch {
	case timestampPattern.MatchString(trimmed), levelPattern.MatchString(trimmed):
		return true
	case strings.HasPrefix(trimmed, "at ") && (strings.Contains(trimmed, "(") || strings.Contains(trimmed, ":")):
		return true
	case strings.HasPrefix(trimmed, "File \""), strings.HasPrefix(trimmed, "Traceback"), strings.HasPrefix(trimmed, "goroutine "),
		strings.HasPrefix(trimmed, "panic:"), strings.HasPrefix(trimmed, "@@"), strings.HasPrefix(trimmed, "+++ "), strings.HasPrefix(trimmed, "--- "),
		strings.HasPrefix(trimmed, "$ "), strings.HasPrefix(trimmed, "❯ "):
		return true
	case (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "\"")) && strings.Contains(trimmed, "\":"):
		return true
	case strings.HasPrefix(trimmed, "}") || strings.HasPrefix(trimmed, "]") || trimmed == "[" || trimmed == "{":
		return true
	case fileLinePattern.MatchString(trimmed) && len(strings.Fields(trimmed)) <= 6:
		return true
	}
	letters := 0
	for _, r := range trimmed {
		if unicode.IsLetter(r) || r == ' ' {
			letters++
		}
	}
	return utf8.RuneCountInString(trimmed) >= 20 && float64(letters)/float64(utf8.RuneCountInString(trimmed)) < 0.6
}

func tableLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) > 1 && strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|")
}

func headingLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "#") && strings.HasPrefix(strings.TrimLeft(trimmed, "#"), " ")
}

func boldBulletLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	trimmed = strings.TrimLeft(trimmed, "-*0123456789.)")
	return strings.HasPrefix(trimmed, " **")
}

// claimStructuredBlocks marks runs of machine output and tables, and the
// region of a message formatted the way agents format replies.
func claimStructuredBlocks(l *labeler, text string, lines [][2]int) {
	claimRuns := func(match func(string) bool, minimum int, reason string) {
		for index := 0; index < len(lines); {
			end := index
			for end < len(lines) && match(text[lines[end][0]:lines[end][1]]) {
				end++
			}
			if end-index >= minimum {
				l.claim(lines[index][0], lines[end-1][1], spanPasted, reason, textOrigin{})
			}
			index = max(end, index+1)
		}
	}
	claimRuns(machineLine, 3, "Log, trace, or data")
	claimRuns(tableLine, 2, "Table")
	first, last, headings, bullets := -1, -1, 0, 0
	for index, line := range lines {
		value := text[line[0]:line[1]]
		heading, bullet := headingLine(value), boldBulletLine(value)
		if heading {
			headings++
		}
		if bullet {
			bullets++
		}
		if heading || bullet {
			if first < 0 {
				first = index
			}
			last = index
		}
	}
	if first >= 0 && (headings >= 2 || bullets >= 3 || headings >= 1 && bullets >= 2) {
		end := last
		for end+1 < len(lines) && strings.TrimSpace(text[lines[end+1][0]:lines[end+1][1]]) != "" {
			end++
		}
		l.claim(lines[first][0], lines[end][1], spanPasted, "Formatted like agent output", textOrigin{})
	}
}

func ago(now time.Time, stamp int64) string {
	return roundDuration(now.Sub(time.Unix(stamp, 0)))
}

func roundDuration(value time.Duration) string {
	switch {
	case value < time.Minute:
		return fmt.Sprintf("%ds", max(int(value.Seconds()), 0))
	case value < time.Hour:
		return fmt.Sprintf("%dm", int(value.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", value.Hours())
	}
}

// authorshipCounts sums characters and words per category.
type authorshipCounts struct {
	chars, words map[string]int
}

func countSpans(text string, spans []authorSpan) authorshipCounts {
	counts := authorshipCounts{chars: map[string]int{}, words: map[string]int{}}
	for _, span := range spans {
		value := text[span.Start:span.End]
		counts.chars[span.Category] += utf8.RuneCountInString(strings.TrimSpace(value))
		counts.words[span.Category] += len(strings.Fields(value))
	}
	return counts
}

// ensureAuthorship starts a background rebuild when messages changed since
// the last one. The tool ledger generation advances whenever a conversation
// is re-ingested or identity links change, which is when authorship can too.
func (c *Catalog) ensureAuthorship() (stale bool) {
	ctx := context.Background()
	generation, err := c.metaValue(ctx, "tool_ledger_generation")
	if err != nil {
		return false
	}
	generation += "/" + authorshipVersion
	built, _ := c.metaValue(ctx, "authorship_generation")
	if generation == built {
		return false
	}
	state := &c.authorship
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.running || built != "" && time.Since(state.builtAt) < authorshipRebuildSpacing {
		return true
	}
	state.running = true
	rebuild := func(ctx context.Context) {
		err := c.RebuildAuthorship(ctx, generation)
		state.mu.Lock()
		state.running, state.builtAt, state.lastError = false, time.Now(), ""
		if err != nil {
			state.lastError = err.Error()
		}
		state.mu.Unlock()
	}
	if c.background == nil {
		go rebuild(ctx)
	} else if !c.background(rebuild) {
		state.running = false // the service is stopping
	}
	return true
}

// authorshipRunning reports whether a rebuild is writing message_authorship.
func (c *Catalog) authorshipRunning() bool {
	c.authorship.mu.Lock()
	defer c.authorship.mu.Unlock()
	return c.authorship.running
}

// RebuildAuthorship classifies every user message of the work each mirror
// group is represented by, in send order, and replaces message_authorship.
func (c *Catalog) RebuildAuthorship(ctx context.Context, generation string) error {
	workspaces, err := queryMapsContext(ctx, c.DB, `SELECT w.id,w.source_kind,w.activity_at FROM workspaces w`)
	if err != nil {
		return err
	}
	suppressed := map[string]bool{}
	for _, row := range c.suppressMirrors(workspaces) {
		if mirrors, ok := row["mirrored_workspace_ids"].([]string); ok {
			for _, id := range mirrors {
				suppressed[id] = true
			}
		}
	}
	inputs, err := c.authorInputs(ctx, suppressed)
	if err != nil {
		return err
	}
	results, err := c.classifyInputs(ctx, inputs)
	if err != nil {
		return err
	}
	return c.storeAuthorship(ctx, results, generation)
}

func (c *Catalog) authorInputs(ctx context.Context, suppressed map[string]bool) ([]authorInput, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT m.id,m.conversation_id,c.workspace_id,m.text,m.created_at,COALESCE(m.sender,''),w.source_kind,c.parent_id IS NOT NULL
		FROM messages m
		JOIN conversations c ON c.id=m.conversation_id JOIN workspaces w ON w.id=c.workspace_id
		WHERE m.kind='message' AND m.role='user' AND m.created_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inputs := []authorInput{}
	for rows.Next() {
		var input authorInput
		var sentAt string
		if err := rows.Scan(&input.ID, &input.ConversationID, &input.WorkspaceID, &input.Text, &sentAt, &input.Sender, &input.SourceKind, &input.SubAgent); err != nil {
			return nil, err
		}
		parsed, ok := parseTime(sentAt)
		if !ok || suppressed[input.WorkspaceID] || strings.TrimSpace(input.Text) == "" {
			continue
		}
		input.SentAt = parsed
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(inputs, func(i, j int) bool {
		if !inputs[i].SentAt.Equal(inputs[j].SentAt) {
			return inputs[i].SentAt.Before(inputs[j].SentAt)
		}
		return inputs[i].ID < inputs[j].ID
	})
	return inputs, nil
}

// classifyInputs walks user messages in send order while streaming agent
// replies in the same order, so the agent index always holds exactly the
// replies of the preceding window.
func (c *Catalog) classifyInputs(ctx context.Context, inputs []authorInput) ([]authorResult, error) {
	results := make([]authorResult, 0, len(inputs))
	if len(inputs) == 0 {
		return results, nil
	}
	rows, err := c.DB.QueryContext(ctx, `SELECT id,conversation_id,created_at,text FROM messages
		WHERE kind='message' AND role='assistant' AND created_at>=? ORDER BY created_at`, formatTime(inputs[0].SentAt.Add(-authorshipWindow)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	classifier := newClassifier(inputs)
	var pending *struct {
		at     time.Time
		origin textOrigin
		text   string
	}
	next := func() error {
		pending = nil
		for rows.Next() {
			var id, conversation, createdAt, text string
			if err := rows.Scan(&id, &conversation, &createdAt, &text); err != nil {
				return err
			}
			if at, ok := parseTime(createdAt); ok {
				pending = &struct {
					at     time.Time
					origin textOrigin
					text   string
				}{at, textOrigin{conversation, id}, text}
				return nil
			}
		}
		return rows.Err()
	}
	if err := next(); err != nil {
		return nil, err
	}
	for _, input := range inputs {
		for pending != nil && pending.at.Before(input.SentAt) {
			classifier.agentText.add(pending.at, pending.origin, shingles(wordTokens(pending.text)))
			if err := next(); err != nil {
				return nil, err
			}
		}
		cutoff := input.SentAt.Add(-authorshipWindow)
		classifier.agentText.evict(cutoff)
		classifier.userText.evict(cutoff)
		results = append(results, authorResult{Input: input, Spans: classifier.classify(input)})
	}
	return results, nil
}

func (c *Catalog) storeAuthorship(ctx context.Context, results []authorResult, generation string) error {
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_authorship"); err != nil {
		return err
	}
	columns := append([]string{"message_id", "conversation_id", "workspace_id", "sent_at", "day", "total_chars"}, authorshipColumns()...)
	columns = append(columns, "spans_json")
	statement, err := tx.PrepareContext(ctx, "INSERT INTO message_authorship("+strings.Join(columns, ",")+") VALUES("+placeholders(len(columns))+")")
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, result := range results {
		input := result.Input
		counts := countSpans(input.Text, result.Spans)
		spans, _ := json.Marshal(result.Spans)
		values := []any{input.ID, input.ConversationID, input.WorkspaceID, formatTime(input.SentAt), input.SentAt.Local().Format("2006-01-02"),
			utf8.RuneCountInString(strings.TrimSpace(input.Text))}
		for _, category := range authorshipCategories {
			values = append(values, counts.chars[category], counts.words[category])
		}
		if _, err := statement.ExecContext(ctx, append(values, string(spans))...); err != nil {
			return err
		}
	}
	for key, value := range map[string]string{"authorship_generation": generation, "authorship_built_at": now()} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuthorshipStats summarizes user-turn text for Settings: all-time and
// last-30-day totals, and a daily series the page rolls up by week or month.
// Every total has characters and words per category.
func (c *Catalog) AuthorshipStats(ctx context.Context) (map[string]any, error) {
	stale := c.ensureAuthorship()
	builtAt, err := c.metaValue(ctx, "authorship_built_at")
	if err != nil {
		return nil, err
	}
	sums := []string{"COUNT(*) messages", "COALESCE(SUM(typed_words>0),0) typed_messages"}
	for _, column := range authorshipColumns() {
		sums = append(sums, fmt.Sprintf("COALESCE(SUM(%s),0) %s", column, column))
	}
	aggregate := strings.Join(sums, ",")
	totals, err := queryMapsContext(ctx, c.DB, "SELECT "+aggregate+",COALESCE(SUM(total_chars),0) total_chars,MIN(day) first_day,MAX(day) last_day FROM message_authorship")
	if err != nil {
		return nil, err
	}
	since := c.clock().AddDate(0, 0, -29).Format("2006-01-02")
	recent, err := queryMapsContext(ctx, c.DB, "SELECT "+aggregate+" FROM message_authorship WHERE day>=?", since)
	if err != nil {
		return nil, err
	}
	daily, err := queryMapsContext(ctx, c.DB, "SELECT day,"+aggregate+" FROM message_authorship GROUP BY day ORDER BY day")
	if err != nil {
		return nil, err
	}
	state := &c.authorship
	state.mu.Lock()
	defer state.mu.Unlock()
	return map[string]any{
		"version": authorshipVersion, "built_at": nilIfEmpty(builtAt), "running": state.running, "stale": stale, "error": nilIfEmpty(state.lastError),
		"window_hours": int(authorshipWindow.Hours()), "totals": totals[0], "last_30_days": recent[0], "daily": daily,
	}, nil
}

// attachAuthorship adds each classified user message's spans, with excerpts,
// so the transcript reader can say where its text came from and link to the
// conversation holding copied text.
func (c *Catalog) attachAuthorship(conversationID any, messages []map[string]any) error {
	rows, err := queryMaps(c.DB, "SELECT message_id,typed_words,pasted_words,spans_json FROM message_authorship WHERE conversation_id=?", conversationID)
	if err != nil || len(rows) == 0 {
		return err
	}
	byID := map[string]map[string]any{}
	decoded := map[string][]authorSpan{}
	sources := map[string]bool{}
	for _, row := range rows {
		id := firstString(row["message_id"])
		var spans []authorSpan
		if json.Unmarshal([]byte(firstString(row["spans_json"])), &spans) != nil {
			continue
		}
		byID[id], decoded[id] = row, spans
		for _, span := range spans {
			if span.Source != "" {
				sources[span.Source] = true
			}
		}
	}
	workspaces := map[string]map[string]any{}
	if len(sources) > 0 {
		ids := make([]any, 0, len(sources))
		for id := range sources {
			ids = append(ids, id)
		}
		found, err := queryMaps(c.DB, `SELECT c.id,c.workspace_id,w.title FROM conversations c JOIN workspaces w ON w.id=c.workspace_id
			WHERE c.id IN (`+placeholders(len(ids))+`)`, ids...)
		if err != nil {
			return err
		}
		for _, row := range found {
			workspaces[firstString(row["id"])] = row
		}
	}
	for _, message := range messages {
		id := firstString(message["id"])
		row := byID[id]
		if row == nil {
			continue
		}
		text := firstString(message["text"])
		parts := []map[string]any{}
		for _, span := range decoded[id] {
			if span.End > len(text) {
				break
			}
			value := strings.TrimSpace(text[span.Start:span.End])
			part := map[string]any{"category": span.Category, "reason": nilIfEmpty(span.Reason), "words": len(strings.Fields(value)), "excerpt": excerpt(value, 120)}
			if source := workspaces[span.Source]; source != nil {
				part["source"] = map[string]any{"workspace_id": source["workspace_id"], "conversation_id": span.Source, "message_id": nilIfEmpty(span.Message), "title": source["title"]}
			}
			parts = append(parts, part)
		}
		message["authorship"] = map[string]any{"typed_words": row["typed_words"], "pasted_words": row["pasted_words"], "spans": parts}
	}
	return nil
}

func excerpt(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}
