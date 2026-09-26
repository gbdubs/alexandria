package archive

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"regexp"
	"sort"
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

// vectorStore keeps the vectors of a documents table, which stores each as
// vector_json, again as float32 blobs in a side table that search reads
// instead (see semantic_vectors in schema.sql).
type vectorStore struct{ documents, key, vectors string }

var (
	workspaceVectors    = vectorStore{"semantic_documents", "workspace_id", "semantic_vectors"}
	conversationVectors = vectorStore{"conversation_documents", "conversation_id", "conversation_vectors"}
)

// store saves the vector of a document, after the document itself.
func (s vectorStore) store(tx *sql.Tx, id string, vector []float64) error {
	_, err := tx.Exec("INSERT INTO "+s.vectors+"("+s.key+",vector) VALUES(?,?) ON CONFLICT("+s.key+") DO UPDATE SET vector=excluded.vector",
		id, vectorBlob(vector))
	return err
}

// missing selects the documents without a stored vector, through the two
// primary key indexes, so no other vector_json is read.
func (s vectorStore) missing() string {
	return "SELECT " + s.key + " FROM " + s.documents + " EXCEPT SELECT " + s.key + " FROM " + s.vectors
}

// vectorBlob encodes a vector for a vector table. float32 keeps far more
// precision than the hashed features carry.
func vectorBlob(vector []float64) []byte {
	blob := make([]byte, 4*len(vector))
	for index, value := range vector {
		binary.LittleEndian.PutUint32(blob[4*index:], math.Float32bits(float32(value)))
	}
	return blob
}

// appendVectorBlob appends the vector blob encodes, so a scan can reuse one slice.
func appendVectorBlob(vector []float64, blob []byte) []float64 {
	for offset := 0; offset+4 <= len(blob); offset += 4 {
		vector = append(vector, float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[offset:]))))
	}
	return vector
}

type semanticMatch struct {
	id    string
	score float64
}

// vectorScores scores every document's vector against query, and returns
// those scoring above minimum. A document without a stored vector is scored
// from its vector_json.
func (c *Catalog) vectorScores(ctx context.Context, store vectorStore, query string, minimum float64) ([]semanticMatch, error) {
	queryVector := semanticEmbed(query)
	rows, err := c.DB.QueryContext(ctx, "SELECT "+store.key+",vector,NULL FROM "+store.vectors+
		" UNION ALL SELECT "+store.key+",NULL,vector_json FROM "+store.documents+" WHERE "+store.key+" IN ("+store.missing()+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	matches := []semanticMatch{}
	var id string
	var blob sql.RawBytes
	var text sql.NullString
	buffer := make([]float64, 0, semanticDimensions)
	for rows.Next() {
		if err := rows.Scan(&id, &blob, &text); err != nil {
			return nil, err
		}
		var vector []float64
		if text.Valid {
			if vector, err = decodeVector(text.String); err != nil {
				continue
			}
		} else {
			buffer = appendVectorBlob(buffer[:0], blob)
			vector = buffer
		}
		if score := semanticCosine(queryVector, vector); score > minimum {
			matches = append(matches, semanticMatch{id, score})
		}
	}
	return matches, rows.Err()
}

// semanticMatches returns, best first, up to limit workspaces whose vectors
// score above minimum against query.
func (c *Catalog) semanticMatches(ctx context.Context, query string, minimum float64, limit int) ([]semanticMatch, error) {
	matches, err := c.vectorScores(ctx, workspaceVectors, query, minimum)
	if err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	return matches[:min(limit, len(matches))], nil
}

// backfillVectors stores the vector of every document without one: all of
// them once after an upgrade, then any an older build rewrote. Each batch
// reads and writes in one transaction, so it cannot replace a vector a
// concurrent ingest just stored with the one that ingest replaced.
func (c *Catalog) backfillVectors(store vectorStore) error {
	for {
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return err
		}
		rows, err := queryMaps(tx, "SELECT "+store.key+" id,vector_json FROM "+store.documents+" WHERE "+store.key+" IN ("+store.missing()+" LIMIT 500)")
		if err != nil || len(rows) == 0 {
			tx.Rollback()
			return err
		}
		for _, row := range rows {
			// Search skips a vector_json it cannot parse; an empty vector matches nothing either.
			vector, _ := decodeVector(row["vector_json"])
			if err := store.store(tx, firstString(row["id"]), vector); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}
