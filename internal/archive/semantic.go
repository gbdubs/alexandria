package archive

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const semanticDimensions = 384

var semanticWords = regexp.MustCompile(`[a-z0-9_./-]+`)
var concepts = map[string]string{"bug": "failure", "bugs": "failure", "error": "failure", "errors": "failure", "crash": "failure", "broken": "failure", "fail": "failure", "failed": "failure", "fix": "repair", "fixed": "repair", "resolve": "repair", "resolved": "repair", "implement": "build", "implemented": "build", "create": "build", "created": "build", "performance": "speed", "slow": "speed", "latency": "speed", "remove": "delete", "removed": "delete", "cleanup": "delete", "reclaim": "delete", "authentication": "auth", "authorization": "auth", "database": "db", "sqlite": "db", "conversation": "session", "thread": "session", "workspace": "worktree", "repository": "repo", "test": "validation", "tests": "validation", "verify": "validation", "verified": "validation"}

func normalizeWord(word string) string {
	if value, ok := concepts[word]; ok {
		return value
	}
	word = strings.TrimSuffix(word, "ing")
	word = strings.TrimSuffix(word, "ed")
	word = strings.TrimSuffix(word, "s")
	return word
}

// semanticToken is one word of a text as written and as the index compares it.
type semanticToken struct{ raw, normalized string }

func semanticTokens(text string) []semanticToken {
	rawWords := semanticWords.FindAllString(strings.ToLower(text), -1)
	tokens := make([]semanticToken, 0, len(rawWords))
	for _, word := range rawWords {
		tokens = append(tokens, semanticToken{word, normalizeWord(word)})
	}
	return tokens
}

// semanticFeatures emits, in order, each feature the embedding hashes: a word
// ("w:"), each character trigram of a word ("c:"), and each adjacent word pair
// ("b:"). token is the index of the word it came from, a pair's first word.
func semanticFeatures(tokens []semanticToken, emit func(feature string, weight float64, token int)) {
	for index, token := range tokens {
		word := token.normalized
		if len(word) > 1 {
			emit("w:"+word, 1, index)
		}
		padded := "^" + word + "$"
		for offset := 0; offset < len(padded)-2; offset++ {
			emit("c:"+padded[offset:offset+3], .18, index)
		}
		if index+1 < len(tokens) {
			emit("b:"+word+":"+tokens[index+1].normalized, .55, index)
		}
	}
}

// semanticBucket is the vector dimension a feature adds to, and whether it
// adds (+1) or subtracts (-1). Different features can share a dimension.
func semanticBucket(feature string) (int, float64) {
	hash, _ := blake2b.New(8, nil)
	_, _ = hash.Write([]byte(feature))
	raw := binary.LittleEndian.Uint64(hash.Sum(nil))
	if raw&(uint64(1)<<63) != 0 {
		return int(raw % semanticDimensions), 1
	}
	return int(raw % semanticDimensions), -1
}

func semanticEmbed(text string) []float64 {
	vector := make([]float64, semanticDimensions)
	semanticFeatures(semanticTokens(text), func(feature string, weight float64, _ int) {
		index, sign := semanticBucket(feature)
		vector[index] += sign * weight
	})
	norm := 0.0
	for _, value := range vector {
		norm += value * value
	}
	norm = math.Sqrt(norm)
	if norm != 0 {
		for index := range vector {
			vector[index] /= norm
		}
	}
	return vector
}
func semanticCosine(left, right []float64) float64 {
	limit := min(len(left), len(right))
	value := 0.0
	for index := 0; index < limit; index++ {
		value += left[index] * right[index]
	}
	return value
}
func decodeVector(value any) ([]float64, error) {
	var vector []float64
	err := json.Unmarshal([]byte(firstString(value)), &vector)
	return vector, err
}
