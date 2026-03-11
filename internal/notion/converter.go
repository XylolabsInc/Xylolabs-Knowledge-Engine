package notion

import (
	"strings"

	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// ConvertPage converts a Notion page and its blocks to a KB document.
func ConvertPage(page NotionPage, blocks []NotionBlock) kb.Document {
	content := blocksToText(blocks, 0)

	return kb.Document{
		Source:      kb.SourceNotion,
		SourceID:    page.ID,
		Title:       page.Title,
		Content:     content,
		ContentType: "page",
		Author:      page.CreatedBy,
		URL:         page.URL,
		Timestamp:   page.CreatedTime,
		UpdatedAt:   page.LastEditedTime,
		Metadata: map[string]string{
			"notion_id": page.ID,
		},
	}
}

func blocksToText(blocks []NotionBlock, depth int) string {
	var sb strings.Builder
	indent := strings.Repeat("  ", depth)

	for _, block := range blocks {
		text := block.Content
		if text == "" && len(block.Children) == 0 {
			continue
		}

		switch block.Type {
		case "heading_1":
			sb.WriteString("# ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case "heading_2":
			sb.WriteString("## ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case "heading_3":
			sb.WriteString("### ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case "bulleted_list_item":
			sb.WriteString(indent)
			sb.WriteString("- ")
			sb.WriteString(text)
			sb.WriteString("\n")
		case "numbered_list_item":
			sb.WriteString(indent)
			sb.WriteString("1. ")
			sb.WriteString(text)
			sb.WriteString("\n")
		case "to_do":
			sb.WriteString(indent)
			sb.WriteString("- [ ] ")
			sb.WriteString(text)
			sb.WriteString("\n")
		case "toggle":
			sb.WriteString(indent)
			sb.WriteString("> ")
			sb.WriteString(text)
			sb.WriteString("\n")
		case "code":
			sb.WriteString("```\n")
			sb.WriteString(text)
			sb.WriteString("\n```\n\n")
		case "quote", "callout":
			sb.WriteString("> ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case "child_page":
			sb.WriteString(indent)
			sb.WriteString("- [Child page: ")
			sb.WriteString(text)
			sb.WriteString("]\n")
		case "child_database":
			sb.WriteString(indent)
			sb.WriteString("- [Database: ")
			sb.WriteString(text)
			sb.WriteString("]\n")
		case "divider":
			sb.WriteString("---\n\n")
		case "table_row":
			sb.WriteString("| ")
			sb.WriteString(text)
			sb.WriteString(" |\n")
		default:
			if text != "" {
				sb.WriteString(indent)
				sb.WriteString(text)
				sb.WriteString("\n\n")
			}
		}

		if len(block.Children) > 0 {
			sb.WriteString(blocksToText(block.Children, depth+1))
		}
	}

	return sb.String()
}

// parseBlock extracts a NotionBlock from raw API JSON.
func parseBlock(block map[string]any) NotionBlock {
	nb := NotionBlock{
		ID:   stringVal(block, "id"),
		Type: stringVal(block, "type"),
	}
	if hc, ok := block["has_children"].(bool); ok {
		nb.HasChild = hc
	}

	// Extract rich text content from the block type
	nb.Content = extractBlockText(block, nb.Type)

	return nb
}

func extractBlockText(block map[string]any, blockType string) string {
	typeData, ok := block[blockType].(map[string]any)
	if !ok {
		return ""
	}

	// child_page and child_database have a "title" string, not rich_text.
	if blockType == "child_page" || blockType == "child_database" {
		if title, ok := typeData["title"].(string); ok {
			return title
		}
		return ""
	}

	// Most block types have a "rich_text" array
	richText, ok := typeData["rich_text"].([]any)
	if !ok {
		// Some blocks use "text" instead
		richText, ok = typeData["text"].([]any)
		if !ok {
			return ""
		}
	}

	return richTextToPlain(richText)
}

// FindChildPages extracts IDs of child_page blocks from a block tree.
func FindChildPages(blocks []NotionBlock) []string {
	var ids []string
	for _, block := range blocks {
		if block.Type == "child_page" {
			ids = append(ids, block.ID)
		}
		if len(block.Children) > 0 {
			ids = append(ids, FindChildPages(block.Children)...)
		}
	}
	return ids
}

// FindChildDatabases extracts IDs of child_database blocks from a block tree.
func FindChildDatabases(blocks []NotionBlock) []string {
	var ids []string
	for _, block := range blocks {
		if block.Type == "child_database" {
			ids = append(ids, block.ID)
		}
		if len(block.Children) > 0 {
			ids = append(ids, FindChildDatabases(block.Children)...)
		}
	}
	return ids
}

func richTextToPlain(richText []any) string {
	var sb strings.Builder
	for _, rt := range richText {
		rtMap, ok := rt.(map[string]any)
		if !ok {
			continue
		}
		if pt, ok := rtMap["plain_text"].(string); ok {
			sb.WriteString(pt)
		}
	}
	return sb.String()
}
