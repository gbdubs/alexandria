# Canonical source export

An export is one workspace object or `{"workspaces": [...]}`. Unknown keys are retained by the producer but safely ignored by this extractor.

```json
{
  "id": "stable-source-workspace-id",
  "source_kind": "tl1",
  "title": "Human-readable purpose",
  "purpose": "Initiating goal",
  "outcome": "Observed result",
  "activity_at": "2026-09-18T12:00:00Z",
  "location": "/exact/worktree/path",
  "repository": {
    "canonical_remote": "https://github.com/owner/repo.git",
    "display_name": "repo",
    "owner": "owner",
    "aliases": [],
    "local_locations": []
  },
  "metadata": {
    "base_ref": "commit", "head_ref": "commit", "branch": "branch",
    "owner": "owner", "flavor": "candidate", "version": "shape-v1",
    "lifecycle": "terminal", "terminal": true, "custody_version": "v4"
  },
  "conversations": [{
    "id": "provider-native-session-id", "provider": "codex", "parent_id": null,
    "aliases": ["conductor-declared-native-id"], "coverage": "complete",
    "messages": [{
      "id": "provider-event-id", "role": "user", "kind": "message",
      "text": "Visible content", "created_at": "2026-09-18T12:00:00Z"
    }]
  }],
  "changes": [{
    "classification": "attempted", "base_id": "…", "head_id": "…",
    "files": [{"path": "src/file.py", "status": "modified", "tracked": true}]
  }],
  "metrics": [{
    "name": "input_tokens", "value": 1200, "unit": "tokens",
    "status": "reported-cumulative", "coverage": 1.0, "extractor_version": "tl1-v1"
  }],
  "prs": [{
    "host": "github.com", "number": 42, "relationship": "commit-evidence",
    "confidence": 1.0, "evidence": {"commit": "…"}
  }]
}
```

Roles and metric absence are preserved. Producers should classify subagent tool calls as `kind: "delegation"`; they are retained even though routine tool traffic is low priority. Never emit hidden reasoning.

