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

// storeSemanticVector saves the vector of a workspace's semantic_documents row
// to semantic_vectors, where search reads it.
func storeSemanticVector(tx *sql.Tx, workspaceID string, vector []float64) error {
	_, err := tx.Exec(`INSERT INTO semantic_vectors(workspace_id,vector) VALUES(?,?)
		ON CONFLICT(workspace_id) DO UPDATE SET vector=excluded.vector`, workspaceID, vectorBlob(vector))
	return err
}

// vectorBlob encodes a vector for semantic_vectors. float32 keeps far more
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

// semanticVectorsSelect reads every workspace's vector: the stored one, or
// vector_json for a workspace without one. Those are found through the two
// primary key indexes, so no other vector_json is read.
const semanticVectorsSelect = `SELECT workspace_id,vector,NULL FROM semantic_vectors
	UNION ALL SELECT workspace_id,NULL,vector_json FROM semantic_documents WHERE workspace_id IN
	(SELECT workspace_id FROM semantic_documents EXCEPT SELECT workspace_id FROM semantic_vectors)`

type semanticMatch struct {
	id    string
	score float64
}

// semanticMatches returns, best first, up to limit workspaces whose vectors
// score above minimum against query.
func (c *Catalog) semanticMatches(ctx context.Context, query string, minimum float64, limit int) ([]semanticMatch, error) {
	queryVector := semanticEmbed(query)
	rows, err := c.DB.QueryContext(ctx, semanticVectorsSelect)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	return matches[:min(limit, len(matches))], nil
}

// backfillSemanticVectors stores the vector of every semantic document without
// one: all of them once after an upgrade, then any an older build rewrote.
// Each batch reads and writes in one transaction, so it cannot replace a
// vector a concurrent ingest just stored with the one that ingest replaced.
func (c *Catalog) backfillSemanticVectors() error {
	for {
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return err
		}
		rows, err := queryMaps(tx, `SELECT workspace_id,vector_json FROM semantic_documents WHERE workspace_id IN
			(SELECT workspace_id FROM semantic_documents EXCEPT SELECT workspace_id FROM semantic_vectors LIMIT 500)`)
		if err != nil || len(rows) == 0 {
			tx.Rollback()
			return err
		}
		for _, row := range rows {
			// Search skips a vector_json it cannot parse; an empty vector matches nothing either.
			vector, _ := decodeVector(row["vector_json"])
			if err := storeSemanticVector(tx, firstString(row["workspace_id"]), vector); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
}
