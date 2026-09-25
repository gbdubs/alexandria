package archive

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Library search ranks each workspace by its best text match and its concept
// similarity. A text match is a message holding every query word (word
// search) or every query term as a substring (substring search); among the
// searchScan best-scoring messages, the workspace with the best message ranks
// first. Concept similarity is the cosine between the query's and the
// workspace's concept vectors, counted above relatedThreshold.
const (
	searchScan       = 500
	relatedThreshold = .05
	relatedWeight    = .6
	textWeight       = .4
)

// searchMatches is how one kind of text match found a workspace.
type searchMatches struct {
	// rank is 1 for the workspace with the best matching message, and so on;
	// 0 means no scanned message matched.
	rank int
	// count is the workspace's matching messages among those scanned.
	count int
	// messages are its best matching messages, best first, at most three.
	messages []string
}

// searchEvidence is what put one workspace in a query's results.
type searchEvidence struct {
	text, substring searchMatches
	related         float64
}

func (m searchMatches) value() float64 {
	if m.rank == 0 {
		return 0
	}
	return 1 / float64(m.rank)
}

func (e *searchEvidence) lexical() float64 { return max(e.text.value(), e.substring.value()) }

func (e *searchEvidence) textWeight() float64 {
	if e.related > 0 {
		return textWeight
	}
	return 1
}

func (e *searchEvidence) score() float64 {
	return e.related*relatedWeight + e.lexical()*e.textWeight()
}

func (e *searchEvidence) matchTypes() []string {
	types := []string{}
	if e.text.rank > 0 {
		types = append(types, "text")
	}
	if e.substring.rank > 0 {
		types = append(types, "substring")
	}
	if e.related > 0 {
		types = append(types, "related")
	}
	return types
}

// searchEvidence finds the workspaces a query matches and why.
func (c *Catalog) searchEvidence(ctx context.Context, options SearchOptions) (map[string]*searchEvidence, error) {
	evidence := map[string]*searchEvidence{}
	get := func(id string) *searchEvidence {
		if evidence[id] == nil {
			evidence[id] = &searchEvidence{}
		}
		return evidence[id]
	}
	collect := func(query string, args []any, pick func(*searchEvidence) *searchMatches) error {
		rows, err := queryMapsContext(ctx, c.DB, query, args...)
		if err != nil {
			return err
		}
		ranked := 0
		for _, row := range rows {
			matches := pick(get(firstString(row["workspace_id"])))
			if matches.rank == 0 {
				ranked++
				matches.rank = ranked
			}
			matches.count++
			if len(matches.messages) < 3 {
				matches.messages = append(matches.messages, firstString(row["message_id"]))
			}
		}
		return nil
	}
	if parsed := ftsQuery(options.Query); parsed != "" {
		if err := collect(`SELECT c.workspace_id,m.id message_id FROM messages_fts
			JOIN messages m ON m.id=messages_fts.message_id JOIN conversations c ON c.id=m.conversation_id
			WHERE messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT ?`, []any{parsed, searchScan},
			func(e *searchEvidence) *searchMatches { return &e.text }); err != nil {
			return nil, err
		}
	}
	if terms, _ := substringTerms(options.Query); options.Substring && len(terms) > 0 {
		if err := collect(`SELECT c.workspace_id,m.id message_id FROM messages_trigram
			JOIN message_fts_rows r ON r.fts_rowid=messages_trigram.rowid JOIN messages m ON m.id=r.message_id
			JOIN conversations c ON c.id=m.conversation_id
			WHERE messages_trigram MATCH ? ORDER BY bm25(messages_trigram) LIMIT ?`, []any{substringQuery(terms), searchScan},
			func(e *searchEvidence) *searchMatches { return &e.substring }); err != nil {
			return nil, err
		}
	}
	queryVector := semanticEmbed(options.Query)
	documents, err := queryMapsContext(ctx, c.DB, "SELECT workspace_id,vector_json FROM semantic_documents")
	if err != nil {
		// Word and substring matches stand without concept similarity.
		return evidence, nil
	}
	type scored struct {
		id    string
		score float64
	}
	scores := []scored{}
	for _, document := range documents {
		vector, decodeErr := decodeVector(document["vector_json"])
		if decodeErr != nil {
			continue
		}
		if score := semanticCosine(queryVector, vector); score > relatedThreshold {
			scores = append(scores, scored{firstString(document["workspace_id"]), score})
		}
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	for _, item := range scores[:min(len(scores), searchScan)] {
		get(item.id).related = item.score
	}
	return evidence, nil
}

// explainLibraryRows adds to each page row found by query a "why": the parts
// of its score, the messages that matched with the matching text marked, and
// which words and fields made it conceptually similar.
func (c *Catalog) explainLibraryRows(ctx context.Context, rows []map[string]any, query string, evidence map[string]*searchEvidence) error {
	found := map[string]*searchEvidence{}
	messageIDs := []any{}
	for _, row := range rows {
		id := firstString(row["id"])
		if e := evidence[id]; e != nil {
			found[id] = e
			for _, messageID := range append(append([]string{}, e.text.messages...), e.substring.messages...) {
				messageIDs = append(messageIDs, messageID)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	wordTerms := words.FindAllString(query, -1)
	substrings, short := substringTerms(query)
	wordSnippets, err := c.matchSnippets(ctx, messageIDs, wordTerms, true)
	if err != nil {
		return err
	}
	substringSnippets, err := c.matchSnippets(ctx, messageIDs, substrings, false)
	if err != nil {
		return err
	}
	related := []string{}
	for id, e := range found {
		if e.related > 0 {
			related = append(related, id)
		}
	}
	documents, err := c.embeddedDocuments(ctx, related)
	if err != nil {
		return err
	}
	for _, row := range rows {
		e := found[firstString(row["id"])]
		if e == nil {
			continue
		}
		why := map[string]any{"score": e.score(), "scan": searchScan}
		parts := []map[string]any{}
		if lexical := e.lexical(); lexical > 0 {
			parts = append(parts, map[string]any{"kind": "text", "value": lexical * e.textWeight(), "weight": e.textWeight()})
		}
		if e.related > 0 {
			parts = append(parts, map[string]any{"kind": "related", "value": e.related * relatedWeight, "weight": relatedWeight})
		}
		why["parts"] = parts
		if e.text.rank > 0 {
			why["text"] = matchesWhy(e.text, wordTerms, nil, wordSnippets)
		}
		if e.substring.rank > 0 {
			why["substring"] = matchesWhy(e.substring, substrings, short, substringSnippets)
		}
		if e.related > 0 {
			stored := documents[firstString(row["id"])]
			explanation := explainSemantic(query, stored.document)
			why["related"] = map[string]any{"cosine": e.related, "threshold": relatedThreshold, "weight": relatedWeight,
				"exact": stored.document.matches(stored.vector), "signal": explanation.Signal, "noise": explanation.Noise, "scattered": explanation.Scattered,
				"terms": explanation.Terms, "fields": explanation.Fields}
		}
		row["why"] = why
	}
	return nil
}

func matchesWhy(matches searchMatches, terms, ignored []string, snippets map[string]map[string]any) map[string]any {
	messages := []map[string]any{}
	for _, id := range matches.messages {
		if snippet := snippets[id]; snippet != nil {
			messages = append(messages, snippet)
		}
	}
	why := map[string]any{"rank": matches.rank, "count": matches.count, "terms": terms, "messages": messages}
	if len(ignored) > 0 {
		why["ignored"] = ignored
	}
	return why
}

// matchSnippets reads, for each message, a short window around the first
// occurrence of each term, merges windows that overlap, and splits them into
// segments marking each match. SQLite's lower() folds only ASCII, so a
// non-ASCII term may find no window, as may a match past the first 200,000
// characters; a message with none shows its start.
func (c *Catalog) matchSnippets(ctx context.Context, ids []any, terms []string, wholeWords bool) (map[string]map[string]any, error) {
	snippets := map[string]map[string]any{}
	if len(ids) == 0 || len(terms) == 0 {
		return snippets, nil
	}
	terms = terms[:min(len(terms), 6)]
	columns := make([]string, len(terms))
	args := make([]any, 0, len(terms)+len(ids))
	for index, term := range terms {
		// Tool output can run to megabytes; look for terms near the top.
		find := "instr(lower(substr(m.text,1,200000)),?" + strconv.Itoa(index+1) + ")"
		columns[index] = find + " p" + strconv.Itoa(index) + ",substr(m.text,max(1," + find + "-" + strconv.Itoa(snippetBefore) + ")," +
			strconv.Itoa(snippetBefore+snippetAfter) + ") w" + strconv.Itoa(index)
		args = append(args, strings.ToLower(term))
	}
	holders := make([]string, len(ids))
	for index := range ids {
		holders[index] = "?" + strconv.Itoa(len(terms)+index+1)
	}
	args = append(args, ids...)
	rows, err := queryMapsContext(ctx, c.DB, `SELECT m.id,m.conversation_id,m.role,m.kind,length(m.text) length,
		substr(m.text,1,`+strconv.Itoa(snippetBefore+snippetAfter)+`) top,`+strings.Join(columns, ",")+`
		FROM messages m WHERE m.id IN (`+strings.Join(holders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		windows := []snippetWindow{}
		for index := range terms {
			if found := int(integer(row["p"+strconv.Itoa(index)])); found > 0 {
				windows = append(windows, snippetWindow{max(1, found-snippetBefore), []rune(firstString(row["w"+strconv.Itoa(index)]))})
			}
		}
		if len(windows) == 0 {
			windows = append(windows, snippetWindow{1, []rune(firstString(row["top"]))})
		}
		snippets[firstString(row["id"])] = map[string]any{
			"message_id": row["id"], "conversation_id": row["conversation_id"], "role": row["role"], "kind": row["kind"],
			"segments": snippetSegments(windows, int(integer(row["length"])), terms, wholeWords),
		}
	}
	return snippets, nil
}

const snippetBefore, snippetAfter = 70, 150

// snippetWindow is text starting at a 1-based character offset in a message.
type snippetWindow struct {
	start int
	text  []rune
}

// snippetSegments merges overlapping windows in message order and marks
// matches, with an ellipsis wherever text is left out.
func snippetSegments(windows []snippetWindow, length int, terms []string, wholeWords bool) []map[string]any {
	sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })
	merged := []snippetWindow{windows[0]}
	for _, window := range windows[1:] {
		last := &merged[len(merged)-1]
		end := last.start + len(last.text)
		if window.start > end {
			merged = append(merged, window)
		} else if tail := window.start + len(window.text); tail > end {
			last.text = append(last.text, window.text[end-window.start:]...)
		}
	}
	segments := []map[string]any{}
	for _, window := range merged {
		segments = append(segments, highlightSegments(string(window.text), terms, wholeWords, window.start > 1, false)...)
	}
	if last := merged[len(merged)-1]; last.start+len(last.text) <= length {
		segments = append(segments, map[string]any{"text": "…"})
	}
	return segments
}

// highlightSegments collapses text's whitespace and splits it into plain and
// matching segments. A whole-word match must not continue a letter or digit.
func highlightSegments(text string, terms []string, wholeWords, leading, trailing bool) []map[string]any {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	lower := make([]rune, len(runes))
	for index, r := range runes {
		lower[index] = unicode.ToLower(r)
	}
	word := func(index int) bool {
		return index >= 0 && index < len(runes) && (unicode.IsLetter(runes[index]) || unicode.IsDigit(runes[index]))
	}
	hit := make([]bool, len(runes))
	for _, term := range terms {
		needle := []rune(strings.ToLower(term))
		for start := 0; start+len(needle) <= len(lower) && len(needle) > 0; start++ {
			if string(lower[start:start+len(needle)]) != string(needle) {
				continue
			}
			if wholeWords && (word(start-1) || word(start+len(needle))) {
				continue
			}
			for index := start; index < start+len(needle); index++ {
				hit[index] = true
			}
		}
	}
	segments := []map[string]any{}
	if leading {
		segments = append(segments, map[string]any{"text": "…"})
	}
	for start := 0; start < len(runes); {
		end := start
		for end < len(runes) && hit[end] == hit[start] {
			end++
		}
		segment := map[string]any{"text": string(runes[start:end])}
		if hit[start] {
			segment["match"] = true
		}
		segments = append(segments, segment)
		start = end
	}
	if trailing {
		segments = append(segments, map[string]any{"text": "…"})
	}
	return segments
}

type embeddedDocument struct {
	document semanticDocument
	vector   []float64
}

// embeddedDocuments rebuilds the text each workspace's concept vector was
// embedded from (see upsertSummary): title, purpose, outcome, first request,
// last reply, and failure excerpts. The summary keeps the first request and
// failures, and the last reply when there is no outcome; otherwise the last
// reply is read again. The stored vector is kept to check the rebuild.
func (c *Catalog) embeddedDocuments(ctx context.Context, ids []string) (map[string]embeddedDocument, error) {
	documents := map[string]embeddedDocument{}
	if len(ids) == 0 {
		return documents, nil
	}
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	rows, err := queryMapsContext(ctx, c.DB, `SELECT w.id,w.title,w.purpose,w.outcome,s.initiation,s.outcome summary_outcome,s.failures,d.vector_json
		FROM workspaces w LEFT JOIN summaries s ON s.workspace_id=w.id LEFT JOIN semantic_documents d ON d.workspace_id=w.id
		WHERE w.id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		id := firstString(row["id"])
		fields := []semanticField{{"Title", firstString(row["title"])}, {"Purpose", firstString(row["purpose"])}}
		lastReply := firstString(row["summary_outcome"])
		if outcome := firstString(row["outcome"]); outcome != "" {
			fields = append(fields, semanticField{"Outcome", outcome})
			replies, err := queryMapsContext(ctx, c.DB, `SELECT CAST(substr(CAST(m.text AS BLOB),1,2000) AS TEXT) text
				FROM conversations mc JOIN messages m ON m.conversation_id=mc.id WHERE mc.workspace_id=? AND +m.role='assistant'
				AND m.kind='message' AND m.text<>'' ORDER BY m.created_at DESC,m.id DESC LIMIT 1`, id)
			if err != nil {
				return nil, err
			}
			lastReply = ""
			if len(replies) > 0 {
				lastReply = firstString(replies[0]["text"])
			}
		}
		fields = append(fields, semanticField{"First ask", firstString(row["initiation"])}, semanticField{"Last reply", lastReply},
			semanticField{"Failure excerpts", firstString(row["failures"])})
		vector, _ := decodeVector(row["vector_json"])
		documents[id] = embeddedDocument{buildSemanticDocument(fields), vector}
	}
	return documents, nil
}
