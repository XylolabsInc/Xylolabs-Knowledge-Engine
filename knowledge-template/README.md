# Xylolabs Knowledge Base

Curated knowledge base auto-generated from Slack, Google Workspace, and Notion.

## Structure

- [`slack/`](slack/) — Slack channel message digests
- [`google/`](google/) — Google Workspace documents
- [`notion/`](notion/) — Notion pages and databases
- [`indexes/`](indexes/) — Topic, people, keyword, and weekly summary indexes
- [`_meta/`](_meta/) — Sync state and document mapping metadata

## How It Works

1. **Go Worker** ([Xylolabs-Knowledge-Engine](https://github.com/XylolabsInc/Xylolabs-Knowledge-Engine)) fetches raw data from Slack, Google Workspace, and Notion into a SQLite database
2. **Cron Job** (`generate-kb.sh`) invokes `kb-gen` (Gemini AI) to transform raw data into structured markdown, with Claude Code CLI as fallback when Gemini quota is exhausted
3. **This repo** stores the curated markdown knowledge base with hierarchy, cross-references, and navigable indexes

## Last Updated

See [`_meta/sync-state.json`](_meta/sync-state.json) for per-source timestamps.
