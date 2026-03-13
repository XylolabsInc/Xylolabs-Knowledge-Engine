package main

import (
	"testing"
)

func TestParseFileBlocks(t *testing.T) {
	input := `Some intro text

===FILE: slack/channels/general/2024-01-15.md===
---
title: "General Channel - January 15"
---

# Daily Digest
Some content here.
===ENDFILE===

===FILE: slack/channels/engineering/2024-01-15.md===
---
title: "Engineering Channel - January 15"
---

# Engineering Digest
More content.
===ENDFILE===
`

	blocks := parseFileBlocks(input)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Path != "slack/channels/general/2024-01-15.md" {
		t.Errorf("block 0 path = %q", blocks[0].Path)
	}
	if blocks[1].Path != "slack/channels/engineering/2024-01-15.md" {
		t.Errorf("block 1 path = %q", blocks[1].Path)
	}
}

func TestParseFileBlocksEmpty(t *testing.T) {
	blocks := parseFileBlocks("no file blocks here")
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestParseFileBlocksNoEndMarker(t *testing.T) {
	input := `===FILE: test.md===
content without end marker`

	blocks := parseFileBlocks(input)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Path != "test.md" {
		t.Errorf("path = %q", blocks[0].Path)
	}
}

func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"#general", "general"},
		{"Engineering Team", "engineering-team"},
		{"Hello World!!!", "hello-world"},
		{"", "general"},
		{"  spaces  ", "spaces"},
	}
	for _, tt := range tests {
		got := sanitizeSlug(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeSlug(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeFilePath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"slack/02_rnd/2024-01-15.md", "slack/02-rnd/2024-01-15.md"},
		{"slack/general/file.md", "slack/general/file.md"},
		{"a_b/c_d/file.md", "a-b/c-d/file.md"},
	}
	for _, tt := range tests {
		got := normalizeFilePath(tt.input)
		if got != tt.want {
			t.Errorf("normalizeFilePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGroupSlackDocuments(t *testing.T) {
	docs := []Document{
		{Channel: "general", Timestamp: "2024-01-15T10:00:00Z"},
		{Channel: "general", Timestamp: "2024-01-15T11:00:00Z"},
		{Channel: "engineering", Timestamp: "2024-01-15T10:00:00Z"},
	}

	batches := groupSlackDocuments(docs, 50)
	if len(batches) != 2 {
		t.Errorf("expected 2 batches, got %d", len(batches))
	}
}

func TestGroupGenericDocuments(t *testing.T) {
	docs := make([]Document, 7)
	batches := groupGenericDocuments(docs, 3)
	if len(batches) != 3 {
		t.Errorf("expected 3 batches, got %d", len(batches))
	}
	if len(batches[2].Documents) != 1 {
		t.Errorf("last batch should have 1 doc, got %d", len(batches[2].Documents))
	}
}
