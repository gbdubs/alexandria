package archive

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// The concept index is feature hashing, not a learned model, so its cosine
// splits exactly into what each query feature shared with the document:
// cosine = Σ over query features f of q_f·d[bucket(f)] / (|q|·|d|). The part
// of d[bucket(f)] from the same feature in the document is real overlap; the
// rest comes from other features hashed to that bucket, which is noise.

// semanticField is one labelled part of the text a document was embedded from.
type semanticField struct{ label, text string }

// semanticDocFeature is one feature's total signed weight in a document.
type semanticDocFeature struct {
	weight   float64
	byField  []float64
	surfaces map[string]int
	// byWord splits a trigram's weight by field for each normalized word it
	// came from, so pieces of the query's own word count as that word.
	byWord map[string][]float64
}

type semanticDocument struct {
	labels   []string
	vector   []float64
	norm     float64
	features map[string]*semanticDocFeature
	// words maps each normalized word to the counts of its written forms.
	words map[string]map[string]int
}

// buildSemanticDocument embeds fields exactly as semanticEmbed embeds their
// newline-joined text: no word spans a newline, so tokenizing each field on
// its own yields the same words, and a pair may span two fields.
func buildSemanticDocument(fields []semanticField) semanticDocument {
	document := semanticDocument{vector: make([]float64, semanticDimensions), features: map[string]*semanticDocFeature{}, words: map[string]map[string]int{}}
	tokens, fieldOf := []semanticToken{}, []int{}
	for index, field := range fields {
		document.labels = append(document.labels, field.label)
		for _, token := range semanticTokens(field.text) {
			tokens, fieldOf = append(tokens, token), append(fieldOf, index)
		}
	}
	for _, token := range tokens {
		if document.words[token.normalized] == nil {
			document.words[token.normalized] = map[string]int{}
		}
		document.words[token.normalized][token.raw]++
	}
	semanticFeatures(tokens, func(name string, weight float64, token int) {
		bucket, sign := semanticBucket(name)
		document.vector[bucket] += sign * weight
		feature := document.features[name]
		if feature == nil {
			feature = &semanticDocFeature{byField: make([]float64, len(fields)), surfaces: map[string]int{}}
			document.features[name] = feature
		}
		feature.weight += sign * weight
		feature.byField[fieldOf[token]] += sign * weight
		switch {
		case strings.HasPrefix(name, "w:"):
			feature.surfaces[tokens[token].raw]++
		case strings.HasPrefix(name, "b:"):
			feature.surfaces[tokens[token].raw+" "+tokens[token+1].raw]++
		default:
			if feature.byWord == nil {
				feature.byWord = map[string][]float64{}
			}
			word := tokens[token].normalized
			if feature.byWord[word] == nil {
				feature.byWord[word] = make([]float64, len(fields))
			}
			feature.byWord[word][fieldOf[token]] += sign * weight
		}
	})
	for _, value := range document.vector {
		document.norm += value * value
	}
	document.norm = math.Sqrt(document.norm)
	return document
}

// matches reports whether the document embeds to stored, the vector the
// index scored: if so, the explanation accounts for the score exactly.
func (d semanticDocument) matches(stored []float64) bool {
	if len(stored) != len(d.vector) || d.norm == 0 {
		return false
	}
	for index, value := range stored {
		if math.Abs(value-d.vector[index]/d.norm) > 1e-9 {
			return false
		}
	}
	return true
}

// relatedTerm is one query word, word pair, or spelling whose features the
// document shares, and what they added to the cosine.
type relatedTerm struct {
	Query   string   `json:"query"`
	Matched []string `json:"matched"`
	// Via is word (the same word), stem (an inflection of it), concept (a
	// word the concept list treats as related), phrase (a word pair), or
	// letters (shared three-letter pieces, which tolerate typos).
	Via     string   `json:"via"`
	Concept string   `json:"concept,omitempty"`
	Value   float64  `json:"value"`
	Fields  []string `json:"fields"`
}

type relatedShare struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type relatedExplanation struct {
	Cosine float64 `json:"cosine"`
	// Signal is what shared features contributed; Noise is what unrelated
	// features hashed to the same dimensions did. They sum to Cosine.
	Signal float64 `json:"signal"`
	Noise  float64 `json:"noise"`
	// Scattered is the part of Signal from three-letter pieces shared with
	// words unlike the query's, which Terms leaves out.
	Scattered float64        `json:"scattered"`
	Terms     []relatedTerm  `json:"terms"`
	Fields    []relatedShare `json:"fields"`
}

var conceptNames = func() map[string]bool {
	names := map[string]bool{}
	for _, concept := range concepts {
		names[concept] = true
	}
	return names
}()

// explainSemantic splits the cosine between query and document into terms.
func explainSemantic(query string, document semanticDocument) relatedExplanation {
	tokens := semanticTokens(query)
	type queryFeature struct {
		name   string
		signed float64
		token  int
		bucket int
	}
	features := []queryFeature{}
	queryVector := make([]float64, semanticDimensions)
	semanticFeatures(tokens, func(name string, weight float64, token int) {
		bucket, sign := semanticBucket(name)
		queryVector[bucket] += sign * weight
		features = append(features, queryFeature{name, sign * weight, token, bucket})
	})
	queryNorm := 0.0
	for _, value := range queryVector {
		queryNorm += value * value
	}
	queryNorm = math.Sqrt(queryNorm)
	explanation := relatedExplanation{Terms: []relatedTerm{}, Fields: []relatedShare{}}
	if queryNorm == 0 || document.norm == 0 {
		return explanation
	}
	scale := queryNorm * document.norm
	type group struct {
		term    relatedTerm
		byField []float64
		letters map[string]bool
	}
	groups := map[string]*group{}
	order := []string{}
	fieldTotals := make([]float64, len(document.labels))
	for _, feature := range features {
		total := feature.signed * document.vector[feature.bucket] / scale
		explanation.Cosine += total
		shared := document.features[feature.name]
		if shared == nil {
			explanation.Noise += total
			continue
		}
		same := feature.signed * shared.weight / scale
		explanation.Signal += same
		explanation.Noise += total - same
		token := tokens[feature.token]
		entry := func(key string, term relatedTerm) *group {
			if groups[key] == nil {
				groups[key] = &group{term: term, byField: make([]float64, len(document.labels)), letters: map[string]bool{}}
				order = append(order, key)
			}
			return groups[key]
		}
		add := func(target *group, byField []float64) {
			for index, weight := range byField {
				value := feature.signed * weight / scale
				target.term.Value += value
				target.byField[index] += value
				fieldTotals[index] += value
			}
		}
		word := func() *group {
			target := entry("w:"+token.normalized, relatedTerm{Query: token.raw, Matched: []string{}, Via: "word"})
			if document.words[token.normalized] != nil {
				target.term.Matched = mergeSurfaces(target.term.Matched, document.words[token.normalized])
			}
			if target.term.Via == "word" && !(len(target.term.Matched) == 1 && target.term.Matched[0] == token.raw) {
				if conceptNames[token.normalized] {
					target.term.Via, target.term.Concept = "concept", token.normalized
				} else {
					target.term.Via = "stem"
				}
			}
			return target
		}
		switch {
		case strings.HasPrefix(feature.name, "w:"):
			add(word(), shared.byField)
		case strings.HasPrefix(feature.name, "b:"):
			target := entry(feature.name, relatedTerm{Query: token.raw + " " + tokens[feature.token+1].raw, Matched: []string{}, Via: "phrase"})
			target.term.Matched = mergeSurfaces(target.term.Matched, shared.surfaces)
			add(target, shared.byField)
		default:
			// Pieces of the query's own word, however it is written, count
			// as that word; pieces of other words are similar spelling.
			rest := slices.Clone(shared.byField)
			if own := shared.byWord[token.normalized]; own != nil {
				add(word(), own)
				for index, weight := range own {
					rest[index] -= weight
				}
			}
			letters := entry("c@"+token.normalized, relatedTerm{Query: token.raw, Matched: []string{}, Via: "letters"})
			letters.letters[strings.TrimPrefix(feature.name, "c:")] = true
			add(letters, rest)
		}
	}
	for _, key := range order {
		entry := groups[key]
		if entry.term.Via == "letters" {
			if entry.term.Matched = document.similarWords(strings.TrimPrefix(key, "c@"), entry.letters); len(entry.term.Matched) == 0 {
				explanation.Scattered += entry.term.Value
				continue
			}
		}
		entry.term.Fields = []string{}
		for _, index := range rankedPositive(entry.byField) {
			entry.term.Fields = append(entry.term.Fields, document.labels[index])
		}
		if math.Abs(entry.term.Value) >= .001 {
			explanation.Terms = append(explanation.Terms, entry.term)
		}
	}
	sort.SliceStable(explanation.Terms, func(i, j int) bool { return explanation.Terms[i].Value > explanation.Terms[j].Value })
	if len(explanation.Terms) > 12 {
		explanation.Terms = explanation.Terms[:12]
	}
	for _, index := range rankedPositive(fieldTotals) {
		explanation.Fields = append(explanation.Fields, relatedShare{document.labels[index], fieldTotals[index]})
	}
	return explanation
}

// mergeSurfaces adds the most frequent written forms to matched, keeping at most four.
func mergeSurfaces(matched []string, surfaces map[string]int) []string {
	forms := make([]string, 0, len(surfaces))
	for form := range surfaces {
		forms = append(forms, form)
	}
	sort.Slice(forms, func(i, j int) bool {
		if surfaces[forms[i]] != surfaces[forms[j]] {
			return surfaces[forms[i]] > surfaces[forms[j]]
		}
		return forms[i] < forms[j]
	})
	for _, form := range forms {
		if len(matched) >= 4 {
			break
		}
		if !slices.Contains(matched, form) {
			matched = append(matched, form)
		}
	}
	return matched
}

// similarWords lists up to three document words that share most of the
// query word's three-letter pieces, closest first.
func (d semanticDocument) similarWords(word string, shared map[string]bool) []string {
	pieces := func(value string) map[string]bool {
		padded, set := "^"+value+"$", map[string]bool{}
		for offset := 0; offset < len(padded)-2; offset++ {
			set[padded[offset:offset+3]] = true
		}
		return set
	}
	query := pieces(word)
	type candidate struct {
		form  string
		score float64
	}
	candidates := []candidate{}
	for normalized, forms := range d.words {
		if normalized == word {
			continue
		}
		other := pieces(normalized)
		common := 0
		for piece := range other {
			if query[piece] && shared[piece] {
				common++
			}
		}
		if common == 0 {
			continue
		}
		score := float64(common) / float64(len(query)+len(other)-common)
		if score < .4 {
			continue
		}
		best, count := "", 0
		for form, n := range forms {
			if n > count || n == count && form < best {
				best, count = form, n
			}
		}
		candidates = append(candidates, candidate{best, score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].form < candidates[j].form
	})
	matched := []string{}
	for _, item := range candidates {
		if len(matched) == 3 {
			break
		}
		matched = append(matched, item.form)
	}
	return matched
}

// rankedPositive returns the indexes of positive values, largest first.
func rankedPositive(values []float64) []int {
	indexes := []int{}
	for index, value := range values {
		if value > 1e-4 {
			indexes = append(indexes, index)
		}
	}
	sort.SliceStable(indexes, func(i, j int) bool { return values[indexes[i]] > values[indexes[j]] })
	return indexes
}
