package archive

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// The substring index (messages_trigram) lets Library search match any three
// or more characters inside a word: "log_que" finds catalog_query. It covers
// conversation prose only, the kinds below. Tool calls, tool output, and
// metadata are most of a catalog's text (about 16 of 17 GB in a large one),
// and a trigram index is larger than the text it covers.
const substringKinds = "'message','result','delegation','delegation_result'"

// substringInsert indexes the prose of the messages where selects. Ingest
// calls it after insertConversationFTS gives them messages_fts rowids.
func substringInsert(where string) string {
	return `INSERT INTO messages_trigram(rowid,text) SELECT r.fts_rowid,m.text FROM messages m
		JOIN message_fts_rows r ON r.message_id=m.id WHERE ` + where + ` AND m.kind IN (` + substringKinds + `) AND m.text<>''`
}

// substringTerms splits a query into the quoted terms of a trigram match and
// the terms too short for one (under three characters), which it ignores.
func substringTerms(query string) (terms, short []string) {
	for _, part := range words.FindAllString(query, -1) {
		if utf8.RuneCountInString(part) < 3 {
			short = append(short, part)
		} else {
			terms = append(terms, part)
		}
	}
	return terms, short
}

func substringQuery(terms []string) string {
	quoted := make([]string, len(terms))
	for index, term := range terms {
		quoted[index] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " AND ")
}

// Conversations indexed before the substring index existed are added in the
// background, a batch of conversations per transaction, in rowid order.
// message_trigram_cursor is the last conversation rowid done;
// message_trigram_version is set once every conversation is. Ingest indexes
// what it writes meanwhile, and a batch deletes its rows before inserting
// them, so a conversation both paths reach is indexed once.
const substringBatch = 100

type substringProgress struct {
	Ready bool `json:"ready"`
	// Done and Total count conversations.
	Done  int `json:"done"`
	Total int `json:"total"`
}

func (c *Catalog) substringIndexProgress(ctx context.Context) (substringProgress, error) {
	var version string
	err := c.DB.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='message_trigram_version'").Scan(&version)
	if err == nil {
		return substringProgress{Ready: true}, nil
	} else if err != sql.ErrNoRows {
		return substringProgress{}, err
	}
	var progress substringProgress
	err = c.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM conversations WHERE rowid<=COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key='message_trigram_cursor'),0)),
		(SELECT COUNT(*) FROM conversations)`).Scan(&progress.Done, &progress.Total)
	return progress, err
}

// indexSubstringBatch indexes the next batch of conversations and reports
// whether any remain.
func (c *Catalog) indexSubstringBatch(ctx context.Context) (bool, error) {
	tx, err := c.beginWrite(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var done string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='message_trigram_version'").Scan(&done); err == nil {
		return false, nil
	} else if err != sql.ErrNoRows {
		return false, err
	}
	var cursor, last sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT CAST(value AS INTEGER) FROM meta WHERE key='message_trigram_cursor'").Scan(&cursor); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT MAX(rowid) FROM (SELECT rowid FROM conversations WHERE rowid>? ORDER BY rowid LIMIT ?)`,
		cursor.Int64, substringBatch).Scan(&last); err != nil {
		return false, err
	}
	if !last.Valid {
		if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('message_trigram_version','1')"); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	batch := "m.conversation_id IN (SELECT id FROM conversations WHERE rowid>? AND rowid<=?)"
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages_trigram WHERE rowid IN (SELECT r.fts_rowid FROM messages m
		JOIN message_fts_rows r ON r.message_id=m.id WHERE `+batch+`)`, cursor.Int64, last.Int64); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, substringInsert(batch), cursor.Int64, last.Int64); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key,value) VALUES('message_trigram_cursor',?)", fmt.Sprint(last.Int64)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// maintainSubstringIndex builds the substring index for conversations indexed
// before it existed, then returns.
func (c *Catalog) maintainSubstringIndex(ctx context.Context) {
	for {
		more, err := c.indexSubstringBatch(ctx)
		wait := 20 * time.Millisecond
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "Substring index: %v\n", err)
			wait = 5 * time.Second
		} else if !more {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
