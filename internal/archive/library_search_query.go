package archive

import "strings"

// libraryQuery separates complete, multi-word quoted phrases from the text
// searched by the usual word, substring, and related-term paths. An unmatched
// quote and a quoted single word keep the old search behavior.
func libraryQuery(query string) (string, []string) {
	var unquoted strings.Builder
	phrases := []string{}
	for len(query) > 0 {
		start := strings.IndexByte(query, '"')
		if start < 0 {
			unquoted.WriteString(query)
			break
		}
		unquoted.WriteString(query[:start])
		query = query[start+1:]
		end := strings.IndexByte(query, '"')
		if end < 0 {
			unquoted.WriteByte(' ')
			unquoted.WriteString(query)
			break
		}
		parts := words.FindAllString(query[:end], -1)
		if len(parts) >= 2 {
			phrases = append(phrases, strings.Join(parts, " "))
		} else {
			unquoted.WriteByte(' ')
			unquoted.WriteString(query[:end])
		}
		unquoted.WriteByte(' ')
		query = query[end+1:]
	}
	return strings.TrimSpace(unquoted.String()), phrases
}

func libraryPhraseQuery(phrases []string) string {
	quoted := make([]string, len(phrases))
	for i, phrase := range phrases {
		quoted[i] = `"` + strings.ReplaceAll(phrase, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " AND ")
}
