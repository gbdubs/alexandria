package archive

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// documentInitiationQuery and documentOutcomeQuery read a conversation's
// first request and last reply. The unary + keeps role out of index choice:
// through messages_prose_idx, SQLite would read every prose message in the
// catalog to find this conversation's.
const (
	documentInitiationQuery = `SELECT m.id,substr(m.text,1,601) text FROM messages m WHERE m.conversation_id=?
		AND +m.role IN ('user','agent') AND m.kind='message' AND m.text<>''
		ORDER BY m.source_order IS NULL,m.source_order,m.created_at,m.id LIMIT 1`
	documentOutcomeQuery = `SELECT m.id,substr(m.text,1,601) text FROM messages m WHERE m.conversation_id=?
		AND +m.role='assistant' AND m.kind='message' AND m.text<>''
		ORDER BY m.source_order IS NULL DESC,m.source_order DESC,m.created_at DESC,m.id DESC LIMIT 1`
)

// Conversation documents are small, extractive discovery aids. The original
// messages remain the source of truth and are fetched only on demand.
func upsertConversationDocument(tx *sql.Tx, conversationID string) error {
	rows, err := queryMaps(tx, documentInitiationQuery, conversationID)
	if err != nil {
		return err
	}
	var initiation, initiationID string
	if len(rows) > 0 {
		initiation = clipText(firstString(rows[0]["text"]), 600)
		initiationID = firstString(rows[0]["id"])
	}
	rows, err = queryMaps(tx, documentOutcomeQuery, conversationID)
	if err != nil {
		return err
	}
	var outcome, outcomeID string
	if len(rows) > 0 {
		outcome = clipText(firstString(rows[0]["text"]), 600)
		outcomeID = firstString(rows[0]["id"])
	}
	metadata, err := queryMaps(tx, `SELECT substr(w.title,1,201) title,substr(w.purpose,1,601) purpose,
		substr(w.outcome,1,601) outcome FROM conversations c
		JOIN workspaces w ON w.id=c.workspace_id WHERE c.id=?`, conversationID)
	if err != nil {
		return err
	}
	if len(metadata) == 0 {
		return fmt.Errorf("conversation not found: %s", conversationID)
	}
	parts := []string{initiation, outcome}
	if initiation == "" && outcome == "" {
		parts = []string{firstString(metadata[0]["title"]), firstString(metadata[0]["purpose"]), firstString(metadata[0]["outcome"])}
	}
	vector := semanticEmbed(strings.Join(parts, "\n"))
	_, err = tx.Exec(`INSERT INTO conversation_documents(conversation_id,initiation,initiation_message_id,outcome,outcome_message_id,vector_json,model,indexed_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(conversation_id) DO UPDATE SET
		initiation=excluded.initiation,initiation_message_id=excluded.initiation_message_id,
		outcome=excluded.outcome,outcome_message_id=excluded.outcome_message_id,
		vector_json=excluded.vector_json,model=excluded.model,indexed_at=excluded.indexed_at`,
		conversationID, nilIfEmpty(initiation), nilIfEmpty(initiationID), nilIfEmpty(outcome), nilIfEmpty(outcomeID),
		jsonText(vector), "local-concept-hash-v1", now())
	return err
}

func (c *Catalog) backfillConversationDocuments() error {
	rows, err := queryMaps(c.DB, `SELECT c.id FROM conversations c LEFT JOIN conversation_documents d ON d.conversation_id=c.id WHERE d.conversation_id IS NULL`)
	if err != nil {
		return err
	}
	for start := 0; start < len(rows); start += 100 {
		tx, err := c.beginWrite(context.Background())
		if err != nil {
			return err
		}
		for _, row := range rows[start:min(start+100, len(rows))] {
			if err := upsertConversationDocument(tx, firstString(row["id"])); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func clipText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}
