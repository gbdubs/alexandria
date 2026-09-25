package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type chatGPTAdapter struct{ baseAdapter }

func (a *chatGPTAdapter) exportFile() string {
	info, err := os.Stat(a.config.Path)
	if err == nil && info.IsDir() {
		return filepath.Join(a.config.Path, "conversations.json")
	}
	return a.config.Path
}
func (a *chatGPTAdapter) Fingerprint() (string, error) {
	file := a.exportFile()
	if _, err := os.Stat(file); err != nil {
		return "", fmt.Errorf("ChatGPT export is missing %s", file)
	}
	fingerprint, err := pathFingerprint(file, func(string, os.DirEntry) bool { return true })
	return "token-events-v2:" + fingerprint, err
}
func (a *chatGPTAdapter) Discover(emit func(WorkspaceRecord) error) error {
	file := a.exportFile()
	raw, version, err := readJSONVersion(file)
	if err != nil {
		return fmt.Errorf("cannot read ChatGPT export %s: %w", file, err)
	}
	rows, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("ChatGPT export %s must contain an array", file)
	}
	for index, rawRow := range rows {
		row, ok := rawRow.(map[string]any)
		if !ok {
			return fmt.Errorf("ChatGPT export %s conversation %d must be an object", file, index)
		}
		mapping, ok := row["mapping"].(map[string]any)
		if !ok {
			return fmt.Errorf("ChatGPT export %s conversation %d has an invalid mapping", file, index)
		}
		messages := []MessageRecord{}
		for nodeID, rawNode := range mapping {
			node, ok := rawNode.(map[string]any)
			if !ok {
				return fmt.Errorf("ChatGPT export %s node %s must be an object", file, nodeID)
			}
			if node["message"] == nil {
				continue
			}
			message, ok := node["message"].(map[string]any)
			if !ok {
				return fmt.Errorf("ChatGPT export %s node %s has an invalid message", file, nodeID)
			}
			content, ok := message["content"].(map[string]any)
			if !ok {
				return fmt.Errorf("ChatGPT export %s node %s has an invalid message", file, nodeID)
			}
			author, ok := message["author"].(map[string]any)
			if !ok {
				return fmt.Errorf("ChatGPT export %s node %s has an invalid author", file, nodeID)
			}
			text := messageText(content["parts"])
			if text != "" {
				messages = append(messages, MessageRecord{RawText: accountingRaw(message), NativeID: defaultString(message["id"], nodeID), Role: defaultString(author["role"], "unknown"), Text: text, Kind: "message", CreatedAt: iso(message["create_time"]), ParentNativeID: firstString(node["parent"]), EvidenceLocator: fmt.Sprintf("%s:mapping:%s", a.original(file), nodeID), Selected: true})
			}
		}
		sort.SliceStable(messages, func(i, j int) bool {
			if messages[i].CreatedAt == messages[j].CreatedAt {
				return messages[i].NativeID < messages[j].NativeID
			}
			return messages[i].CreatedAt < messages[j].CreatedAt
		})
		nativeID := firstString(row["conversation_id"], row["id"])
		if nativeID == "" {
			return fmt.Errorf("ChatGPT export %s conversation %d has no ID", file, index)
		}
		record := WorkspaceRecord{Observed: version, SourceID: nativeID, SourceKind: "chatgpt", Title: defaultString(row["title"], "ChatGPT "+short(nativeID)), Account: a.config.Account, ActivityAt: iso(row["update_time"]), Metadata: map[string]any{}, Conversations: []ConversationRecord{{NativeID: nativeID, Provider: "chatgpt", Account: a.config.Account, Origin: "user-export", Coverage: "complete", Messages: messages, StartedAt: iso(row["create_time"]), EndedAt: iso(row["update_time"]), Observed: version}}}
		if err := emit(record); err != nil {
			return err
		}
	}
	return nil
}
