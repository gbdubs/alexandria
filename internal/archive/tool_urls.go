package archive

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// toolURL is one address a tool call reached or was shown. Source says where
// it was read: "input" (a tool argument such as WebFetch's url or a browser
// navigation), "command" (a shell command that talks to the network),
// "result" (a page the tool reports opening), or "search_result" (a link a
// web search returned, which the agent did not necessarily visit).
type toolURL struct {
	Position          int
	URL, Host, Source string
}

const maxToolURLs = 50

var (
	urlPattern = regexp.MustCompile(`(?i)\b(?:https?|wss?|ftp)://[^\s"'<>` + "`" + `\\|{}^\[\]]+`)
	// networkSignal marks a line of a shell command or script that talks to
	// the network. URLs elsewhere (a PR body, an echo) are not visits.
	networkSignal = regexp.MustCompile(`(?i)(?:^|[\s;&|(` + "`" + `$"'=])(?:curl|wget|xh|aria2c|lynx|w3m|websocat|grpcurl|git\s+(?:clone|fetch|pull|push|ls-remote|submodule|remote\s+(?:add|set-url)))\s` +
		`|(?:^|[;&|(]\s*)(?:open|xdg-open)\s+['"]?(?:https?|wss?)://` +
		`|urlopen\(|urllib\.request|\brequests\.(?:get|post|put|patch|delete|head|request|Session)\b|\bhttpx\.|\baiohttp\b|\bfetch\(|\baxios\b` +
		`|\bhttps?\.(?:get|request)\(|\.goto\(|webbrowser\.open|Invoke-(?:WebRequest|RestMethod)|\bnew\s+WebSocket\(`)
	assignmentPattern = regexp.MustCompile(`^\s*(?:export\s+|local\s+|readonly\s+|const\s+|let\s+|var\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*:?=`)
	identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	openedPagePattern = regexp.MustCompile(`(?m)^Web search completed for: (\S+)`)
	urlInputKeys      = map[string]bool{"url": true, "urls": true, "uri": true, "href": true, "link": true, "target_url": true, "page_url": true, "website": true}
)

// cleanToolURL keeps network URLs with a literal host and returns the host
// (with any port) so calls group by site.
func cleanToolURL(raw string) (string, string) {
	raw = strings.TrimRight(strings.TrimSpace(raw), ".,:;!?)'\"")
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss", "ftp":
	default:
		return "", ""
	}
	host := strings.ToLower(parsed.Host)
	if host == "" || strings.ContainsAny(host, "$%{}<>()") || strings.HasSuffix(host, ":") {
		return "", ""
	}
	return clipText(raw, 2000), host
}

func (c *toolCall) addURL(raw, source string) {
	address, host := cleanToolURL(raw)
	if address == "" || len(c.URLs) >= maxToolURLs {
		return
	}
	for _, existing := range c.URLs {
		if existing.URL == address {
			return
		}
	}
	c.URLs = append(c.URLs, toolURL{Position: len(c.URLs), URL: address, Host: host, Source: source})
}

// applyToolURLs records the addresses a call reached: URL arguments, network
// commands in shell calls, pages a web tool reports opening, and the links a
// web search returned. The first reached URL and the reached hosts are kept
// on the call for filtering; every URL is listed in tool_urls.
func applyToolURLs(call *toolCall, input any, content string) {
	collectInputURLs(call, input, 0)
	for _, text := range call.commandTexts {
		for _, address := range commandURLs(text) {
			call.addURL(address, "command")
		}
	}
	call.SearchQuery = searchQuery(input)
	if call.Category == "web" {
		for _, match := range openedPagePattern.FindAllStringSubmatch(content, -1) {
			call.addURL(match[1], "result")
		}
		if call.SearchQuery != "" || strings.Contains(strings.ToLower(call.ToolName), "search") {
			for _, address := range urlPattern.FindAllString(content, -1) {
				call.addURL(address, "search_result")
			}
		}
	}
	hosts := []string{}
	seen := map[string]bool{}
	for _, entry := range call.URLs {
		if entry.Source == "search_result" {
			continue
		}
		if call.URL == "" {
			call.URL, call.Host = entry.URL, entry.Host
		}
		call.URLCount++
		if !seen[entry.Host] {
			seen[entry.Host] = true
			hosts = append(hosts, entry.Host)
		}
	}
	call.Hosts = strings.Join(hosts, " ")
}

// collectInputURLs reads URL-named arguments at any depth (Codex web search
// actions nest theirs). Other string arguments are not scanned: a prompt or a
// file's contents may mention sites the call never reached.
func collectInputURLs(call *toolCall, value any, depth int) {
	if depth > 4 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if urlInputKeys[strings.ToLower(key)] {
				collectURLValues(call, typed[key])
			} else {
				collectInputURLs(call, typed[key], depth+1)
			}
		}
	case []any:
		for _, item := range typed {
			collectInputURLs(call, item, depth+1)
		}
	}
}

func collectURLValues(call *toolCall, value any) {
	switch typed := value.(type) {
	case string:
		call.addURL(typed, "input")
	case []any:
		for _, item := range typed {
			collectURLValues(call, item)
		}
	}
}

// searchQuery reads a web search's query from the input shapes providers use.
func searchQuery(input any) string {
	fields := mapValue(input)
	if fields == nil {
		return ""
	}
	query := firstString(fields["query"], fields["q"], fields["search_query"])
	if query == "" {
		if action := mapValue(fields["action"]); action != nil {
			query = firstString(action["query"])
		}
	}
	if query == "" {
		if searches, ok := fields["search_query"].([]any); ok && len(searches) > 0 {
			query = firstString(mapValue(searches[0])["q"])
		}
	}
	return clipText(strings.TrimSpace(query), 500)
}

// commandURLs returns the URLs a shell command or script sends requests to.
// A URL counts when its line runs a network client (curl, wget, git clone,
// urlopen, fetch, ...), or when it is assigned to a variable that such
// a line uses, directly or through other variables (BASE=https://...; curl
// "$BASE/x"). Lines ending in a backslash, an open parenthesis, or a comma
// continue onto the next.
func commandURLs(text string) []string {
	if !strings.Contains(text, "://") {
		return nil
	}
	lines := []string{}
	var current strings.Builder
	for _, line := range strings.Split(text, "\n") {
		current.WriteString(line)
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasSuffix(trimmed, "\\") || strings.HasSuffix(trimmed, "(") || strings.HasSuffix(trimmed, ",") {
			current.WriteString(" ")
			continue
		}
		lines = append(lines, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	used := make([]bool, len(lines))
	names := map[string]bool{}
	pending := []int{}
	for index, line := range lines {
		if networkSignal.MatchString(line) {
			used[index] = true
			pending = append(pending, index)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	assigned := make([]string, len(lines))
	for index, line := range lines {
		if match := assignmentPattern.FindStringSubmatch(line); len(match) == 2 {
			assigned[index] = match[1]
		}
	}
	// Follow variable references back to their assignments.
	for len(pending) > 0 {
		index := pending[0]
		pending = pending[1:]
		for _, name := range identifierPattern.FindAllString(lines[index], -1) {
			if names[name] {
				continue
			}
			names[name] = true
			for other, variable := range assigned {
				if variable == name && !used[other] {
					used[other] = true
					pending = append(pending, other)
				}
			}
		}
	}
	urls := []string{}
	for index, line := range lines {
		if used[index] {
			urls = append(urls, urlPattern.FindAllString(line, -1)...)
		}
	}
	return urls
}
