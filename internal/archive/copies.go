package archive

import (
	"context"
	"database/sql"
)

// A conversation can reach the catalog from several copies: the same session
// on two Macs (native session IDs are global), or in two sources on one Mac.
// The copy that last wrote all of a conversation, named by its host and
// origin, stays authoritative for it exactly as a single source always was,
// including pruning. Any other copy takes over only when it holds every stored
// message unchanged, so taking over can never drop anything; otherwise it may
// only add messages the catalog lacks. Once copies diverge no copy holds every
// message, and the conversation stays append-only until one does.

// mergedWriter marks a conversation whose messages are a union of diverging
// copies, so no single copy may prune it.
const mergedWriter = "merged"

type storedConversation struct{ id, origin, writer string }

type copyAuthority struct {
	// workspace reports whether this copy may replace the workspace's
	// record-derived data (metrics, summary) rather than extend it.
	workspace     bool
	conversations []conversationAuthority
	// older is set for a capture's copy older in some part than what its host
	// already wrote (see guardCapture); it writes only its newer conversations.
	older bool
}

type conversationAuthority struct {
	id     string
	writer bool
	stored map[string]bool // stored native IDs, when the copy may only add
	older  bool            // left alone, see guardCapture
}

func storedWorkspaceID(tx *sql.Tx, record WorkspaceRecord) (string, error) {
	var id string
	err := tx.QueryRow("SELECT id FROM workspaces WHERE source_kind=? AND source_account=? AND source_id=?", record.SourceKind, record.Account, record.SourceID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func storedConversations(tx *sql.Tx, record WorkspaceRecord) ([]storedConversation, error) {
	stored := make([]storedConversation, len(record.Conversations))
	for index, conversation := range record.Conversations {
		err := tx.QueryRow(`SELECT id,COALESCE(origin,''),COALESCE(origin_host_id,(SELECT value FROM meta WHERE key=?),'')
			FROM conversations WHERE provider=? AND account=? AND native_id=?`, legacyHostKey, conversation.Provider, conversation.Account, conversation.NativeID).
			Scan(&stored[index].id, &stored[index].origin, &stored[index].writer)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
	}
	return stored, nil
}

func resolveAuthority(tx *sql.Tx, record WorkspaceRecord, workspaceID, host string) (copyAuthority, error) {
	authority := copyAuthority{workspace: true, conversations: make([]conversationAuthority, len(record.Conversations))}
	stored, err := storedConversations(tx, record)
	if err != nil {
		return authority, err
	}
	carried := map[string]bool{}
	for index, conversation := range record.Conversations {
		claim := &authority.conversations[index]
		claim.id = stored[index].id
		carried[claim.id] = true
		if claim.id == "" || (stored[index].writer == host && stored[index].origin == conversation.Origin) {
			claim.writer = true
			continue
		}
		covered, ids, err := coversStoredMessages(tx, claim.id, conversation.Messages)
		if err != nil {
			return authority, err
		}
		claim.writer = covered
		if !covered {
			claim.stored = ids
			authority.workspace = false
		}
	}
	// A new workspace has nothing to regress. An existing one is replaced from
	// this copy only if the copy also carries every conversation another host
	// wrote; absent ones from this host keep single-source behaviour.
	if workspaceID == "" {
		authority.workspace = true
		return authority, nil
	}
	if !authority.workspace {
		return authority, nil
	}
	rows, err := queryMaps(tx, `SELECT id,COALESCE(origin_host_id,(SELECT value FROM meta WHERE key=?),'') writer
		FROM conversations WHERE workspace_id=?`, legacyHostKey, workspaceID)
	if err != nil {
		return authority, err
	}
	for _, row := range rows {
		if !carried[firstString(row["id"])] && firstString(row["writer"]) != host {
			authority.workspace = false
			break
		}
	}
	return authority, nil
}

// coverCheckHook observes each cover check, which reads every stored message
// of a conversation; tests count them.
var coverCheckHook func(conversationID string)

// coversStoredMessages reports whether a copy holds every stored message of a
// conversation unchanged, and returns the stored native IDs.
func coversStoredMessages(tx *sql.Tx, conversationID string, messages []MessageRecord) (bool, map[string]bool, error) {
	if coverCheckHook != nil {
		coverCheckHook(conversationID)
	}
	incoming := make(map[string]string, len(messages))
	for _, message := range messages {
		incoming[message.NativeID] = hashBytes([]byte(message.Text))
	}
	rows, err := tx.Query("SELECT native_id,content_hash FROM messages WHERE conversation_id=?", conversationID)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	stored := map[string]bool{}
	covered := true
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return false, nil, err
		}
		stored[id] = true
		covered = covered && incoming[id] == hash
	}
	return covered, stored, rows.Err()
}

// mergeWorkspace lets a non-writer copy fill gaps and extend activity without
// replacing what the writer recorded.
func mergeWorkspace(tx *sql.Tx, workspaceID string, record WorkspaceRecord, repoID string, metadata map[string]any) error {
	activity := nilIfEmpty(record.ActivityAt)
	_, err := tx.Exec(`UPDATE workspaces SET repository_id=COALESCE(repository_id,?),purpose=COALESCE(purpose,?),outcome=COALESCE(outcome,?),
		location=COALESCE(location,?),head_ref=COALESCE(head_ref,?),branch=COALESCE(branch,?),
		activity_at=CASE WHEN ? IS NOT NULL AND (activity_at IS NULL OR ?>activity_at) THEN ? ELSE activity_at END,
		activity_source=CASE WHEN ? IS NOT NULL AND (activity_at IS NULL OR ?>=activity_at) THEN ? ELSE activity_source END,
		indexed_at=? WHERE id=?`,
		nilIfEmpty(repoID), nilIfEmpty(record.Purpose), nilIfEmpty(record.Outcome), nilIfEmpty(record.Location),
		nilIfEmpty(firstString(metadata["head_ref"])), nilIfEmpty(firstString(metadata["branch"])),
		activity, activity, activity, activity, activity, record.SourceKind+":native", now(), workspaceID)
	return err
}

// mergeConversation adds the messages a non-writer copy has that the catalog
// lacks and widens the conversation's span; stored messages are untouched.
func mergeConversation(tx *sql.Tx, claim conversationAuthority, value ConversationRecord) (bool, error) {
	started, ended := nilIfEmpty(value.StartedAt), nilIfEmpty(value.EndedAt)
	if _, err := tx.Exec(`UPDATE conversations SET model=COALESCE(model,?),started_at=COALESCE(MIN(started_at,?),started_at,?),
		ended_at=COALESCE(MAX(ended_at,?),ended_at,?) WHERE id=?`, nilIfEmpty(value.Model), started, started, ended, ended, claim.id); err != nil {
		return false, err
	}
	added := false
	for sourceOrder, message := range value.Messages {
		if claim.stored[message.NativeID] {
			continue
		}
		message.SourceOrder = sourceOrder
		if _, _, err := upsertMessage(tx, claim.id, message); err != nil {
			return false, err
		}
		added = true
	}
	if !added {
		return false, nil
	}
	if _, err := tx.Exec("UPDATE conversations SET origin_host_id=? WHERE id=?", mergedWriter, claim.id); err != nil {
		return false, err
	}
	if err := deleteConversationFTS(tx, claim.id); err != nil {
		return false, err
	}
	if err := insertConversationFTS(tx, claim.id); err != nil {
		return false, err
	}
	return true, upsertConversationDocument(tx, claim.id)
}

// rederiveWorkspace recomputes workspace totals from the stored union after a
// merge. Reconciling interleaved copies is order-sensitive, so a union that
// reports fewer tokens than are stored keeps the stored figures.
func rederiveWorkspace(tx *sql.Tx, workspaceID string, merged map[string]bool) error {
	rows, err := queryMaps(tx, "SELECT id,provider,model,started_at,ended_at,agent_depth FROM conversations WHERE workspace_id=? ORDER BY started_at,id", workspaceID)
	if err != nil {
		return err
	}
	union := WorkspaceRecord{}
	for _, row := range rows {
		id := firstString(row["id"])
		messages, err := storedMessages(context.Background(), tx, id)
		if err != nil {
			return err
		}
		conversation := ConversationRecord{Provider: firstString(row["provider"]), Model: firstString(row["model"]), StartedAt: firstString(row["started_at"]),
			EndedAt: firstString(row["ended_at"]), AgentDepth: int(integer(row["agent_depth"])), Messages: messages}
		union.Conversations = append(union.Conversations, conversation)
		if !merged[id] {
			continue
		}
		var stored float64
		if err := tx.QueryRow("SELECT COALESCE(SUM(total_tokens),0) FROM agent_sessions WHERE conversation_id=?", id).Scan(&stored); err != nil {
			return err
		}
		if conversationTokenCounts(messages)["total_tokens"] >= stored {
			if err := replaceAgentSessions(tx, workspaceID, id, conversation); err != nil {
				return err
			}
		}
		// The ledger is derived from the stored messages, which now include
		// the ones the merge added.
		if err := replaceToolLedger(tx, workspaceID, id, conversation); err != nil {
			return err
		}
	}
	var stored sql.NullFloat64
	if err := tx.QueryRow("SELECT MAX(value) FROM metrics WHERE workspace_id=? AND unit='tokens' AND name='total_tokens'", workspaceID).Scan(&stored); err != nil {
		return err
	}
	if tokens := reconciledTokenMetrics(union); len(tokens) > 0 && metricValue(tokens, "total_tokens") >= stored.Float64 {
		if _, err := tx.Exec("DELETE FROM metrics WHERE workspace_id=? AND unit='tokens'", workspaceID); err != nil {
			return err
		}
		if err := replaceMetrics(tx, workspaceID, tokens); err != nil {
			return err
		}
	}
	return replaceMetrics(tx, workspaceID, derivedMetrics(union))
}

func metricValue(metrics []map[string]any, name string) float64 {
	for _, metric := range metrics {
		if firstString(metric["name"]) == name {
			value, _ := number(metric["value"])
			return value
		}
	}
	return 0
}

// sightCopiedRecord handles a record another host already ingested with the
// same digest, e.g. a source Migration Assistant copied: the catalog holds it
// already, so only this host's sighting is recorded.
func sightCopiedRecord(tx *sql.Tx, record WorkspaceRecord, digest, host, source string) (bool, error) {
	var copies int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM source_record_states WHERE source_kind=? AND source_account=? AND source_id=?
		AND digest=? AND host_id<>?`, record.SourceKind, record.Account, record.SourceID, digest, host).Scan(&copies); err != nil || copies == 0 {
		return false, err
	}
	workspaceID, err := storedWorkspaceID(tx, record)
	if err != nil || workspaceID == "" {
		return false, err
	}
	stored, err := storedConversations(tx, record)
	if err != nil {
		return false, err
	}
	ids := make([]string, len(stored))
	for index, conversation := range stored {
		if conversation.id == "" {
			return false, nil
		}
		ids[index] = conversation.id
	}
	return true, recordSightings(tx, workspaceID, ids, record, host, source, true)
}

// recordSightings updates the workspace's sighting only when the copy wrote
// its fields (an older copy does not); a conversation ID "" was not written.
func recordSightings(tx *sql.Tx, workspaceID string, conversationIDs []string, record WorkspaceRecord, host, source string, workspace bool) error {
	timestamp := now()
	update := `DO UPDATE SET location=COALESCE(excluded.location,workspace_sightings.location),
		repository_locations_json=excluded.repository_locations_json,last_seen_at=excluded.last_seen_at`
	if !workspace {
		update = "DO NOTHING"
	}
	if _, err := tx.Exec(`INSERT INTO workspace_sightings(workspace_id,host_id,source_name,location,repository_locations_json,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(workspace_id,host_id,source_name) `+update,
		workspaceID, host, source, nilIfEmpty(record.Location), jsonText(localLocations(record.Repository)), timestamp, timestamp); err != nil {
		return err
	}
	for index, conversation := range record.Conversations {
		if conversationIDs[index] == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO conversation_sightings(conversation_id,host_id,origin,source_name,messages,ended_at,first_seen_at,last_seen_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(conversation_id,host_id,origin) DO UPDATE SET source_name=excluded.source_name,
			messages=excluded.messages,ended_at=excluded.ended_at,last_seen_at=excluded.last_seen_at`,
			conversationIDs[index], host, conversation.Origin, source, len(conversation.Messages), nilIfEmpty(conversation.EndedAt), timestamp, timestamp); err != nil {
			return err
		}
	}
	return nil
}

func localLocations(repository map[string]any) []string {
	if locations, ok := repository["local_locations"].([]string); ok {
		return uniqueStrings(locations)
	}
	return uniqueStrings(stringSlice(repository["local_locations"]))
}
