"""Backfill Codex model changes from turn_context events without reindexing text.

Usage: python3 tools/backfill_codex_models.py PATH_TO_CATALOG
"""

from __future__ import annotations

import argparse
import bisect
import json
import sqlite3
from pathlib import Path


def model_changes(path: Path) -> tuple[list[int], list[str]]:
    lines: list[int] = []
    models: list[str] = []
    with path.open("rb") as source:
        for number, raw in enumerate(source, 1):
            if b"turn_context" not in raw and b"session_meta" not in raw:
                continue
            try:
                event = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if event.get("type") not in {"turn_context", "session_meta"}:
                continue
            model = event.get("payload", {}).get("model")
            if isinstance(model, str) and model and (not models or model != models[-1]):
                lines.append(number)
                models.append(model)
    return lines, models


def backfill(db: sqlite3.Connection) -> tuple[int, int, int]:
    db.execute("PRAGMA busy_timeout=10000")
    columns = {row[1] for row in db.execute("PRAGMA table_info(messages)")}
    if "model" not in columns:
        db.execute("ALTER TABLE messages ADD COLUMN model TEXT")
    conversations = db.execute(
        "SELECT c.id,c.origin FROM conversations c JOIN workspaces w ON w.id=c.workspace_id "
        "WHERE w.source_kind='codex' AND c.provider='codex' AND c.origin IS NOT NULL"
    ).fetchall()
    updated_conversations = updated_messages = missing = 0
    for conversation_id, origin in conversations:
        path = Path(origin)
        if not path.is_file():
            missing += 1
            continue
        lines, models = model_changes(path)
        if not models:
            continue
        db.execute(
            "UPDATE conversations SET model=? WHERE id=? AND (model IS NULL OR model<>?)",
            (models[-1], conversation_id, models[-1]),
        )
        updated_conversations += 1
        rows = db.execute(
            "SELECT source_order,evidence_locator FROM messages WHERE conversation_id=? ORDER BY source_order",
            (conversation_id,),
        ).fetchall()
        ranges: list[tuple[str, int, int]] = []
        current_model: str | None = None
        start = end = 0
        prefix = origin + ":"
        for source_order, locator in rows:
            if source_order is None or not locator or not locator.startswith(prefix):
                continue
            try:
                line = int(locator[len(prefix):])
            except ValueError:
                continue
            index = bisect.bisect_right(lines, line) - 1
            model = models[index] if index >= 0 else None
            if model is None:
                continue
            if current_model != model or source_order != end + 1:
                if current_model is not None:
                    ranges.append((current_model, start, end))
                current_model, start = model, source_order
            end = source_order
        if current_model is not None:
            ranges.append((current_model, start, end))
        for model, first, last in ranges:
            cursor = db.execute(
                "UPDATE messages SET model=? WHERE conversation_id=? AND source_order BETWEEN ? AND ? "
                "AND (model IS NULL OR model<>?)",
                (model, conversation_id, first, last, model),
            )
            updated_messages += cursor.rowcount
        if updated_conversations % 100 == 0:
            db.commit()
            print(f"{updated_conversations} conversations, {updated_messages} messages", flush=True)
    db.commit()
    return updated_conversations, updated_messages, missing


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("catalog", type=Path)
    args = parser.parse_args()
    with sqlite3.connect(args.catalog) as connection:
        conversations, messages, missing = backfill(connection)
    print(f"Backfilled {conversations} Codex conversations and {messages} messages; {missing} source files missing.")
