package archive

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// LibraryFind searches the evidence itself. Text results are grouped by the
// conversation that owns each message; file and URL results retain their
// provenance instead of inheriting a workspace's semantic score.
type LibraryFindOptions struct {
	Kind, Query                      string
	Fuzzy, CaseSensitive, Separators bool
	Limit, Offset                    int
	limited                          *bool
}

type libraryHit struct {
	WorkspaceID    string `json:"workspace_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	Title          string `json:"title"`
	Provider       string `json:"provider,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	Role           string `json:"role,omitempty"`
	MessageKind    string `json:"message_kind,omitempty"`
	Snippet        string `json:"snippet,omitempty"`
	Path           string `json:"path,omitempty"`
	OldPath        string `json:"old_path,omitempty"`
	URL            string `json:"url,omitempty"`
	URLSource      string `json:"url_source,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	Attribution    string `json:"attribution,omitempty"`
	Count          int    `json:"count"`
}

var libraryWords = regexp.MustCompile(`[\pL\pN]+`)

func quoteFTS(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (c *Catalog) LibraryFind(ctx context.Context, o LibraryFindOptions) (map[string]any, error) {
	o.Query = strings.TrimSpace(o.Query)
	if o.Query == "" {
		return map[string]any{"items": []libraryHit{}, "total": 0}, nil
	}
	if len(o.Query) > 500 {
		return nil, fmt.Errorf("search query is too long (maximum 500 characters)")
	}
	limit := clamp(o.Limit, 1, 100)
	offset := max(o.Offset, 0)
	limited := false
	o.limited = &limited
	var hits []libraryHit
	var err error
	switch o.Kind {
	case "", "text":
		hits, err = c.findText(ctx, o)
	case "file":
		hits, err = c.findFiles(ctx, o)
	case "url":
		hits, err = c.findURLs(ctx, o)
	default:
		return nil, fmt.Errorf("unknown search kind %q", o.Kind)
	}
	if err != nil {
		return nil, err
	}
	total := len(hits)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	return map[string]any{"items": hits[offset:end], "total": total, "limit": limit, "offset": offset, "limited": limited}, nil
}

// A small edit distance supplements FTS word lookup. This works on vocabulary
// terms, so a typo need not force a scan of every message body.
func editDistance(a, b string, maxDistance int) int {
	x, y := []rune(a), []rune(b)
	if max(len(x)-len(y), len(y)-len(x)) > maxDistance {
		return maxDistance + 1
	}
	previous := make([]int, len(y)+1)
	for j := range previous {
		previous[j] = j
	}
	for i, left := range x {
		current := make([]int, len(y)+1)
		current[0] = i + 1
		minimum := current[0]
		for j, right := range y {
			cost := 0
			if left != right {
				cost = 1
			}
			current[j+1] = min(previous[j+1]+1, current[j]+1, previous[j]+cost)
			minimum = min(minimum, current[j+1])
		}
		if minimum > maxDistance {
			return maxDistance + 1
		}
		previous = current
	}
	return previous[len(y)]
}

func fuzzyRoot(s string) string {
	s = strings.ToLower(s)
	// Deceive and deception have different Latin stems. Treat their common
	// family explicitly rather than claiming edit distance relates them.
	if strings.HasPrefix(s, "deceiv") || strings.HasPrefix(s, "deciev") || strings.HasPrefix(s, "decept") || strings.HasPrefix(s, "deceit") {
		return "deceive"
	}
	for _, suffix := range []string{"ation", "ments", "ment", "ingly", "ing", "edly", "ed", "ies", "es", "s"} {
		if strings.HasSuffix(s, suffix) && len(s) > len(suffix)+3 {
			return strings.TrimSuffix(s, suffix)
		}
	}
	return s
}

func sameCasing(a, b string) bool {
	// Exact spellings with different case must never become fuzzy hits.
	if strings.ToLower(a) == strings.ToLower(b) {
		return a == b
	}
	style := func(s string) string {
		letters := []rune(s)
		upper := 0
		for _, r := range letters {
			if unicode.IsUpper(r) {
				upper++
			}
		}
		if upper == 0 {
			return "lower"
		}
		if upper == len(letters) {
			return "upper"
		}
		if unicode.IsUpper(letters[0]) && upper == 1 {
			return "title"
		}
		return "mixed:" + s
	}
	return style(a) == style(b)
}

func (c *Catalog) fuzzyTerms(ctx context.Context, word string) ([]string, error) {
	terms := []string{strings.ToLower(word)}
	if len([]rune(word)) < 4 {
		return terms, nil
	}
	// Restrict expansion to the first two letters and the edit-distance length
	// band. Do not cap the vocabulary scan: common prefixes can have more than
	// 10,000 terms, with the actual correction beyond the first 10,000.
	prefix := strings.ToLower(string([]rune(word)[:2]))
	maxDistance := 1
	if len([]rune(word)) >= 7 {
		maxDistance = 2
	}
	rows, err := c.DB.QueryContext(ctx, `SELECT term FROM messages_vocab WHERE term>=? AND term<? AND length(term) BETWEEN ? AND ?`,
		prefix, prefix+"\uffff", len([]rune(word))-maxDistance, len([]rune(word))+maxDistance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		term     string
		distance int
	}
	candidates := []candidate{}
	seen := map[string]bool{terms[0]: true}
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, err
		}
		if seen[term] {
			continue
		}
		distance := editDistance(strings.ToLower(word), term, maxDistance)
		if fuzzyRoot(word) == fuzzyRoot(term) {
			distance = 0
		}
		if distance <= maxDistance {
			candidates = append(candidates, candidate{term, distance})
			seen[term] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].distance == candidates[j].distance {
			return candidates[i].term < candidates[j].term
		}
		return candidates[i].distance < candidates[j].distance
	})
	for _, item := range candidates[:min(24, len(candidates))] {
		terms = append(terms, item.term)
	}
	return terms, nil
}

func (c *Catalog) findText(ctx context.Context, o LibraryFindOptions) ([]libraryHit, error) {
	phrase := len(o.Query) >= 2 && strings.HasPrefix(o.Query, `"`) && strings.HasSuffix(o.Query, `"`)
	query := o.Query
	if phrase {
		query = strings.TrimSuffix(strings.TrimPrefix(query, `"`), `"`)
	}
	words := libraryWords.FindAllString(query, -1)
	if len(words) == 0 {
		return []libraryHit{}, nil
	}
	clauses := make([]string, 0, len(words))
	if phrase {
		clauses = append(clauses, quoteFTS(strings.Join(words, " ")))
	} else {
		for _, word := range words {
			terms := []string{strings.ToLower(word)}
			if o.Fuzzy {
				var err error
				terms, err = c.fuzzyTerms(ctx, word)
				if err != nil {
					return nil, err
				}
			}
			quoted := make([]string, len(terms))
			for i, term := range terms {
				quoted[i] = quoteFTS(term)
			}
			clauses = append(clauses, "("+strings.Join(quoted, " OR ")+")")
		}
	}
	fts := strings.Join(clauses, " AND ")
	rows, err := queryMapsContext(ctx, c.DB, `SELECT m.id message_id,m.text,m.role,m.kind,m.conversation_id,
		c.provider,c.started_at,w.id workspace_id,w.title FROM messages_fts f
		JOIN messages m ON m.id=f.message_id JOIN conversations c ON c.id=m.conversation_id
		JOIN workspaces w ON w.id=c.workspace_id WHERE messages_fts MATCH ?
		AND m.selected=1 AND m.kind IN ('message','thinking','reasoning','analysis')
		AND (m.role IN ('user','assistant') OR m.kind IN ('thinking','reasoning','analysis'))
		ORDER BY bm25(messages_fts),m.created_at DESC LIMIT 5001`, fts)
	if err != nil {
		return nil, err
	}
	if len(rows) > 5000 {
		*o.limited = true
		rows = rows[:5000]
	}
	groups := map[string]*libraryHit{}
	ordered := []libraryHit{}
	for _, row := range rows {
		body := firstString(row["text"])
		start, end, ok := textMatch(body, query, words, phrase, o)
		if !ok {
			continue
		}
		id := firstString(row["conversation_id"])
		hit := groups[id]
		if hit == nil {
			hit = &libraryHit{WorkspaceID: firstString(row["workspace_id"]), ConversationID: id, Title: firstString(row["title"]), Provider: firstString(row["provider"]), StartedAt: firstString(row["started_at"]), MessageID: firstString(row["message_id"]), Role: firstString(row["role"]), MessageKind: firstString(row["kind"]), Snippet: searchSnippet(body, start, end)}
			groups[id] = hit
			ordered = append(ordered, *hit)
		}
		hit.Count++
	}
	for i := range ordered {
		ordered[i].Count = groups[ordered[i].ConversationID].Count
	}
	return ordered, nil
}

func textMatch(body, query string, words []string, phrase bool, o LibraryFindOptions) (int, int, bool) {
	if phrase {
		if o.Separators {
			haystack, needle := body, query
			if !o.CaseSensitive {
				haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
			}
			at := strings.Index(haystack, needle)
			return at, at + len(query), at >= 0
		}
		parts := make([]string, len(words))
		for i, word := range words {
			parts[i] = regexp.QuoteMeta(word)
		}
		pattern := strings.Join(parts, `[^\pL\pN]+`)
		if !o.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		at := regexp.MustCompile(pattern).FindStringIndex(body)
		if at == nil {
			return 0, 0, false
		}
		return at[0], at[1], true
	}
	// FTS has already required every query term. Verify casing and surface a
	// useful excerpt; fuzzy matches are checked against whole text tokens.
	matchStart, matchEnd := -1, -1
	for _, word := range words {
		found := false
		for _, index := range libraryWords.FindAllStringIndex(body, -1) {
			term := body[index[0]:index[1]]
			a, b := term, word
			if !o.CaseSensitive {
				a, b = strings.ToLower(a), strings.ToLower(b)
			}
			if a == b || (o.Fuzzy && (!o.CaseSensitive || sameCasing(a, b)) && (fuzzyRoot(a) == fuzzyRoot(b) || editDistance(a, b, 2) <= func() int {
				if len([]rune(b)) >= 7 {
					return 2
				}
				return 1
			}())) {
				found = true
				if matchStart < 0 {
					matchStart, matchEnd = index[0], index[1]
				}
				break
			}
		}
		if !found {
			return 0, 0, false
		}
	}
	return matchStart, matchEnd, true
}

func searchSnippet(body string, start, end int) string {
	runes := []rune(body)
	start = len([]rune(body[:min(max(start, 0), len(body))]))
	end = len([]rune(body[:min(max(end, 0), len(body))]))
	lo, hi := max(0, start-90), min(len(runes), end+130)
	text := strings.TrimSpace(string(runes[lo:hi]))
	if lo > 0 {
		text = "…" + text
	}
	if hi < len(runes) {
		text += "…"
	}
	return text
}

func (c *Catalog) findFiles(ctx context.Context, o LibraryFindOptions) ([]libraryHit, error) {
	rows, err := queryMapsContext(ctx, c.DB, `WITH matching_changes AS (
		SELECT cf.path,cf.old_path,cs.id change_set_id,cs.workspace_id,cs.created_at,cs.attempt_id
		FROM change_files cf JOIN change_sets cs ON cs.id=cf.change_set_id
		WHERE instr(lower(cf.path),lower(?))>0 OR instr(lower(COALESCE(cf.old_path,'')),lower(?))>0
		ORDER BY cs.created_at DESC LIMIT 5001
	) SELECT cf.path,cf.old_path,cf.change_set_id,cf.workspace_id,cf.created_at,cf.attempt_id,w.title,
		c.id conversation_id,c.provider,c.started_at,c.ended_at,c.work_item_id
		FROM matching_changes cf JOIN workspaces w ON w.id=cf.workspace_id
		LEFT JOIN conversations c ON c.workspace_id=w.id`, o.Query, o.Query)
	if err != nil {
		return nil, err
	}
	// A change set has workspace scope. Link it to a conversation only when
	// its work item, time interval, or the sole conversation identifies one.
	byWork := map[string][]map[string]any{}
	for _, row := range rows {
		key := firstString(row["change_set_id"]) + "\x00" + firstString(row["path"])
		byWork[key] = append(byWork[key], row)
	}
	if len(byWork) > 5000 {
		*o.limited = true
	}
	hits := []libraryHit{}
	for _, group := range byWork {
		row := group[0]
		chosen := []map[string]any{}
		if len(group) == 1 && firstString(row["conversation_id"]) != "" {
			chosen = group
		}
		if len(chosen) == 0 && firstString(row["attempt_id"]) != "" {
			var workItem string
			_ = c.DB.QueryRowContext(ctx, `SELECT work_item_id FROM task_attempts WHERE id=?`, row["attempt_id"]).Scan(&workItem)
			for _, candidate := range group {
				if workItem != "" && firstString(candidate["work_item_id"]) == workItem {
					chosen = append(chosen, candidate)
				}
			}
		}
		if len(chosen) == 0 {
			at := firstString(row["created_at"])
			for _, candidate := range group {
				if at != "" && firstString(candidate["started_at"]) != "" && firstString(candidate["started_at"]) <= at && (firstString(candidate["ended_at"]) == "" || at <= firstString(candidate["ended_at"])) {
					chosen = append(chosen, candidate)
				}
			}
		}
		if len(chosen) != 1 {
			chosen = []map[string]any{row}
			if len(group) > 1 {
				chosen[0] = map[string]any{}
				for k, v := range row {
					chosen[0][k] = v
				}
				chosen[0]["conversation_id"] = ""
			}
		}
		for _, candidate := range chosen {
			hit := libraryHit{WorkspaceID: firstString(row["workspace_id"]), ConversationID: firstString(candidate["conversation_id"]), Title: firstString(row["title"]), Provider: firstString(candidate["provider"]), Path: firstString(row["path"]), OldPath: firstString(row["old_path"]), Count: 1}
			if hit.ConversationID == "" {
				hit.Attribution = "workspace"
			} else {
				hit.Attribution = "conversation"
			}
			hits = append(hits, hit)
		}
	}
	unique := map[string]*libraryHit{}
	result := []libraryHit{}
	for _, hit := range hits {
		key := hit.WorkspaceID + "\x00" + hit.ConversationID + "\x00" + hit.Path
		if existing := unique[key]; existing != nil {
			existing.Count++
			continue
		}
		copy := hit
		unique[key] = &copy
		result = append(result, hit)
	}
	for i := range result {
		hit := &result[i]
		hit.Count = unique[hit.WorkspaceID+"\x00"+hit.ConversationID+"\x00"+hit.Path].Count
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Title+result[i].Path+result[i].ConversationID < result[j].Title+result[j].Path+result[j].ConversationID
	})
	return result, nil
}

func (c *Catalog) findURLs(ctx context.Context, o LibraryFindOptions) ([]libraryHit, error) {
	needle := o.Query
	if !o.CaseSensitive {
		needle = strings.ToLower(needle)
	}
	comparison := "instr(lower(u.url),?)>0 OR instr(lower(COALESCE(u.host,'')),?)>0"
	if o.CaseSensitive {
		comparison = "instr(u.url,?)>0 OR instr(COALESCE(u.host,''),?)>0"
	}
	rows, err := queryMapsContext(ctx, c.DB, `SELECT u.url,u.source,t.id tool_call_id,t.tool_name,t.call_message_id,t.conversation_id,
		t.workspace_id,w.title,c.provider,c.started_at FROM tool_urls u JOIN tool_calls t ON t.id=u.tool_call_id
		JOIN conversations c ON c.id=t.conversation_id JOIN workspaces w ON w.id=t.workspace_id
		WHERE u.source<>'search_result' AND (`+comparison+`) ORDER BY t.started_at DESC LIMIT 5001`, needle, needle)
	if err != nil {
		return nil, err
	}
	if len(rows) > 5000 {
		*o.limited = true
		rows = rows[:5000]
	}
	hits := []libraryHit{}
	for _, row := range rows {
		hits = append(hits, libraryHit{WorkspaceID: firstString(row["workspace_id"]), ConversationID: firstString(row["conversation_id"]), Title: firstString(row["title"]), Provider: firstString(row["provider"]), StartedAt: firstString(row["started_at"]), MessageID: firstString(row["call_message_id"]), URL: firstString(row["url"]), URLSource: firstString(row["source"]), ToolName: firstString(row["tool_name"]), ToolCallID: firstString(row["tool_call_id"]), Count: 1})
	}
	return hits, nil
}
