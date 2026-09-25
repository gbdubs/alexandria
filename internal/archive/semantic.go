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
func semanticEmbed(text string) []float64 {
	rawWords := semanticWords.FindAllString(strings.ToLower(text), -1)
	normalized := make([]string, 0, len(rawWords))
	for _, word := range rawWords {
		normalized = append(normalized, normalizeWord(word))
	}
	vector := make([]float64, semanticDimensions)
	add := func(feature string, weight float64) {
		hash, _ := blake2b.New(8, nil)
		_, _ = hash.Write([]byte(feature))
		raw := binary.LittleEndian.Uint64(hash.Sum(nil))
		index := raw % semanticDimensions
		if raw&(uint64(1)<<63) != 0 {
			vector[index] += weight
		} else {
			vector[index] -= weight
		}
	}
	for index, word := range normalized {
		if len(word) > 1 {
			add("w:"+word, 1)
		}
		padded := "^" + word + "$"
		for offset := 0; offset < len(padded)-2; offset++ {
			add("c:"+padded[offset:offset+3], .18)
		}
		if index+1 < len(normalized) {
			add("b:"+word+":"+normalized[index+1], .55)
		}
	}
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
