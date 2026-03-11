package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

// ToolExecutor manages available tools and dispatches function calls.
type ToolExecutor struct {
	googleWriter *GoogleWriter
	notionWriter *NotionWriter
	logger       *slog.Logger

	mu          sync.Mutex
	attachments map[string][]byte // file name → data, from Slack file downloads
}

// NewToolExecutor creates a ToolExecutor.
// Either writer can be nil if not configured.
func NewToolExecutor(gw *GoogleWriter, nw *NotionWriter, logger *slog.Logger) *ToolExecutor {
	return &ToolExecutor{
		googleWriter: gw,
		notionWriter: nw,
		logger:       logger.With("component", "tool-executor"),
		attachments:  make(map[string][]byte),
	}
}

// SetAttachments stores file attachments from the current Slack message
// for use by upload_to_drive.
func (e *ToolExecutor) SetAttachments(attachments map[string][]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachments = attachments
}

// ClearAttachments removes stored attachments after processing.
func (e *ToolExecutor) ClearAttachments() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachments = make(map[string][]byte)
}

// Declarations returns the function declarations for all available tools.
func (e *ToolExecutor) Declarations() []gemini.FunctionDeclaration {
	var decls []gemini.FunctionDeclaration

	if e.googleWriter != nil {
		decls = append(decls,
			gemini.FunctionDeclaration{
				Name:        "create_google_doc",
				Description: "Create a new Google Docs document in Google Drive. Use for creating text documents such as meeting notes, reports, and memos.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "Document title",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Document content (Markdown formatting supported: headings, bold, lists, links, etc.)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive folder ID (defaults to shared drive root if empty)",
						},
					},
					"required": []string{"title", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_drive_folder",
				Description: "Create a new folder in Google Drive. Useful for organizing files before uploading them into a structured folder.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"description": "Folder name",
						},
						"parent_folder_id": map[string]any{
							"type":        "string",
							"description": "Parent folder ID (defaults to shared drive root if empty)",
						},
					},
					"required": []string{"name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "upload_to_drive",
				Description: "Upload attached files to Google Drive. Specify file_name to upload a single file, or leave empty to upload all attachments at once.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_name": map[string]any{
							"type":        "string",
							"description": "File name to upload (uploads all attachments if empty)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive folder ID (defaults to shared drive root if empty)",
						},
					},
					"required": []string{},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_drive_file",
				Description: "Delete a file or folder from Google Drive.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file/folder to delete",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "rename_drive_file",
				Description: "Rename a file or folder in Google Drive.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file/folder to rename",
						},
						"new_name": map[string]any{
							"type":        "string",
							"description": "New name",
						},
					},
					"required": []string{"file_id", "new_name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_google_doc",
				Description: "Replace the content of an existing Google Docs document.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Doc to edit",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "New content (Markdown formatting supported: headings, bold, lists, links, etc. Replaces existing content)",
						},
					},
					"required": []string{"file_id", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "search_drive",
				Description: "Search for files in Google Drive by name.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Search query (text contained in file name)",
						},
					},
					"required": []string{"query"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "get_drive_file_info",
				Description: "Retrieve metadata (name, type, URL, modification date) for a Google Drive file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive file ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_doc",
				Description: "Read the content of a Google Docs document as text.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Doc",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_sheet",
				Description: "Read data from a Google Sheets spreadsheet.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "Range to read (e.g. 'Sheet1', 'Sheet1!A1:D10'). Reads all of 'Sheet1' if empty",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_google_sheet",
				Description: "Create a new Google Sheets spreadsheet. Can include initial data.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "Spreadsheet title",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "Initial data (JSON 2D array, e.g. [[\"Name\",\"Age\"],[\"Alice\",\"30\"]]). Creates empty sheet if empty",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive folder ID (defaults to shared drive root if empty)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_google_sheet",
				Description: "Write data to a specific range in Google Sheets (overwrites existing data).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "Range to write (e.g. 'Sheet1!A1:C3')",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "Data (JSON 2D array, e.g. [[\"Name\",\"Age\"],[\"Alice\",\"30\"]])",
						},
					},
					"required": []string{"file_id", "range", "data"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "append_google_sheet",
				Description: "Append new rows to a Google Sheets spreadsheet.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "Location to append (e.g. 'Sheet1')",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "Row data to append (JSON 2D array, e.g. [[\"Alice\",\"30\"],[\"Bob\",\"25\"]])",
						},
					},
					"required": []string{"file_id", "data"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_slides",
				Description: "Read the content of a Google Slides presentation as text.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Slides presentation",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_google_slides",
				Description: "Create a new Google Slides presentation.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "Presentation title",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive folder ID (defaults to shared drive root if empty)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_slide",
				Description: "Add a new slide to an existing Google Slides presentation.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Slides presentation",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "Slide title",
						},
						"body": map[string]any{
							"type":        "string",
							"description": "Slide body content",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "move_drive_file",
				Description: "Move a Google Drive file to a different folder.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file to move",
						},
						"new_folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the destination folder",
						},
					},
					"required": []string{"file_id", "new_folder_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "copy_drive_file",
				Description: "Copy a Google Drive file. Useful for creating new documents based on a template.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file to copy",
						},
						"new_name": map[string]any{
							"type":        "string",
							"description": "Name for the copy (defaults to 'Copy of: original name' if empty)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Folder ID for the copy (defaults to shared drive root if empty)",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_drive_folder",
				Description: "List files inside a Google Drive folder.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the folder to list",
						},
					},
					"required": []string{"folder_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "append_to_google_doc",
				Description: "Append content to the end of an existing Google Docs document. Use for accumulating meeting notes, adding memos, etc.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Doc",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Content to append (added to end of document)",
						},
					},
					"required": []string{"file_id", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "get_sheet_metadata",
				Description: "Retrieve metadata for a Google Sheets spreadsheet (sheet tab names, row/column counts, etc.).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "clear_google_sheet",
				Description: "Delete data in a specific range of a Google Sheets spreadsheet.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "Range to clear (e.g. 'Sheet1!A1:C3')",
						},
					},
					"required": []string{"file_id", "range"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "share_drive_file",
				Description: "Share a Google Drive file with a specific user.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file to share",
						},
						"email": map[string]any{
							"type":        "string",
							"description": "Email address of the person to share with",
						},
						"role": map[string]any{
							"type":        "string",
							"description": "Permission level: reader (view), writer (edit), commenter (comment). Defaults to reader",
						},
					},
					"required": []string{"file_id", "email"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_slide",
				Description: "Delete a slide from a Google Slides presentation.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Slides presentation",
						},
						"slide_id": map[string]any{
							"type":        "string",
							"description": "ID of the slide to delete (deletes last slide if empty)",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_sheet_tab",
				Description: "Add a new sheet tab to a Google Sheets spreadsheet.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Drive ID of the Google Sheets spreadsheet",
						},
						"tab_name": map[string]any{
							"type":        "string",
							"description": "New sheet tab name",
						},
					},
					"required": []string{"file_id", "tab_name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "export_as_pdf",
				Description: "Export a Google Drive file (Docs, Sheets, Slides) as PDF and save to Drive.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive ID of the file to export as PDF",
						},
						"file_name": map[string]any{
							"type":        "string",
							"description": "PDF file name (defaults to 'export.pdf' if empty)",
						},
					},
					"required": []string{"file_id"},
				},
			},
			// --- Calendar Tools ---
			gemini.FunctionDeclaration{
				Name:        "create_calendar_event",
				Description: "Create a new Google Calendar event. Use for scheduling meetings, conferences, and appointments.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary": map[string]any{
							"type":        "string",
							"description": "Event title/summary",
						},
						"start_time": map[string]any{
							"type":        "string",
							"description": "Start time (RFC3339 format: 2024-01-15T09:00:00+09:00, or date-only for all-day: 2024-01-15)",
						},
						"end_time": map[string]any{
							"type":        "string",
							"description": "End time (RFC3339 format: 2024-01-15T10:00:00+09:00, or date-only for all-day: 2024-01-16)",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (defaults to primary calendar if empty)",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "Event description/notes",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "Location",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "Attendee email addresses (comma-separated, e.g. a@co.com,b@co.com)",
						},
					},
					"required": []string{"summary", "start_time", "end_time"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_calendar_event",
				Description: "Edit an existing Google Calendar event. Can modify title, time, location, and attendees.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "Event ID to edit",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (defaults to primary)",
						},
						"summary": map[string]any{
							"type":        "string",
							"description": "New event title",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "New event description",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "New location",
						},
						"start_time": map[string]any{
							"type":        "string",
							"description": "New start time (RFC3339)",
						},
						"end_time": map[string]any{
							"type":        "string",
							"description": "New end time (RFC3339)",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "New attendee list (comma-separated, replaces existing list)",
						},
					},
					"required": []string{"event_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_calendar_event",
				Description: "Delete a Google Calendar event.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "Event ID to delete",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (defaults to primary)",
						},
					},
					"required": []string{"event_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_calendar_events",
				Description: "List Google Calendar events. Use to check schedules within a specific time range.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (defaults to primary)",
						},
						"time_min": map[string]any{
							"type":        "string",
							"description": "Query start time (RFC3339)",
						},
						"time_max": map[string]any{
							"type":        "string",
							"description": "Query end time (RFC3339)",
						},
						"max_results": map[string]any{
							"type":        "number",
							"description": "Maximum results (default: 20)",
						},
					},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_event_attendees",
				Description: "Add attendees to an existing calendar event. Invitation emails are sent automatically.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "Event ID",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "Attendee emails to add (comma-separated)",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (defaults to primary)",
						},
					},
					"required": []string{"event_id", "attendees"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_calendars",
				Description: "List accessible Google Calendars.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{},
				},
			},
			// --- Tasks Tools ---
			gemini.FunctionDeclaration{
				Name:        "create_task",
				Description: "Create a new Google Tasks item.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "Task title",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "Task list ID (defaults to default list if empty)",
						},
						"notes": map[string]any{
							"type":        "string",
							"description": "Notes/details",
						},
						"due": map[string]any{
							"type":        "string",
							"description": "Due date (RFC3339 or YYYY-MM-DD)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_task",
				Description: "Edit an existing Google Tasks item. Can modify title, notes, due date, and completion status.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{
							"type":        "string",
							"description": "Task ID to edit",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "Task list ID (defaults to default list if empty)",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "New title",
						},
						"notes": map[string]any{
							"type":        "string",
							"description": "New notes",
						},
						"due": map[string]any{
							"type":        "string",
							"description": "New due date",
						},
						"status": map[string]any{
							"type":        "string",
							"description": "Status change (needsAction: incomplete, completed: done)",
						},
					},
					"required": []string{"task_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_task",
				Description: "Delete a Google Tasks item.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{
							"type":        "string",
							"description": "Task ID to delete",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "Task list ID (defaults to default list if empty)",
						},
					},
					"required": []string{"task_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_tasks",
				Description: "List Google Tasks items.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "Task list ID (defaults to default list if empty)",
						},
						"max_results": map[string]any{
							"type":        "number",
							"description": "Maximum results (default: 20)",
						},
					},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_task_lists",
				Description: "List Google Tasks task lists.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{},
				},
			},
			// --- Gmail Tool ---
			gemini.FunctionDeclaration{
				Name:        "send_email",
				Description: "Send an email. Use for work-related emails, invitations, and notifications.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"to": map[string]any{
							"type":        "string",
							"description": "Recipient email address (comma-separated for multiple)",
						},
						"subject": map[string]any{
							"type":        "string",
							"description": "Email subject",
						},
						"body": map[string]any{
							"type":        "string",
							"description": "Email body",
						},
						"cc": map[string]any{
							"type":        "string",
							"description": "CC email address (comma-separated for multiple)",
						},
					},
					"required": []string{"to", "subject", "body"},
				},
			},
		)
	}

	if e.notionWriter != nil {
		decls = append(decls,
			gemini.FunctionDeclaration{
				Name:        "create_notion_page",
				Description: "Create a new Notion page. Use for writing meeting notes, documents, and memos in Notion.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "Page title",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Page content (plain text, supports markdown headings/lists)",
						},
						"parent_page_id": map[string]any{
							"type":        "string",
							"description": "Parent page ID (defaults to root page if empty)",
						},
					},
					"required": []string{"title", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "update_notion_page",
				Description: "Append content to an existing Notion page. Use to add new content to a page that already exists.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page_id": map[string]any{
							"type":        "string",
							"description": "Notion page ID",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Content to append (plain text, supports markdown headings/lists)",
						},
					},
					"required": []string{"page_id", "content"},
				},
			},
		)
	}

	return decls
}

// Execute runs a function call and returns the result.
func (e *ToolExecutor) Execute(ctx context.Context, call gemini.FunctionCall) gemini.FunctionResponse {
	e.logger.Info("executing tool", "name", call.Name, "args", call.Args)

	result, err := e.dispatch(ctx, call)
	if err != nil {
		e.logger.Error("tool execution failed", "name", call.Name, "error", err)
		return gemini.FunctionResponse{
			Name:     call.Name,
			Response: map[string]any{"error": err.Error()},
		}
	}

	return gemini.FunctionResponse{
		Name:     call.Name,
		Response: result,
	}
}

func (e *ToolExecutor) dispatch(ctx context.Context, call gemini.FunctionCall) (any, error) {
	switch call.Name {
	case "create_google_doc":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		title, _ := call.Args["title"].(string)
		content, _ := call.Args["content"].(string)
		folderID, _ := call.Args["folder_id"].(string)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		url, err := e.googleWriter.CreateDoc(ctx, title, content, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "title": title}, nil

	case "create_drive_folder":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		name, _ := call.Args["name"].(string)
		parentFolderID, _ := call.Args["parent_folder_id"].(string)
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		folderID, url, err := e.googleWriter.CreateFolder(ctx, name, parentFolderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"folder_id": folderID, "url": url, "name": name}, nil

	case "upload_to_drive":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileName, _ := call.Args["file_name"].(string)
		folderID, _ := call.Args["folder_id"].(string)

		e.mu.Lock()
		allFiles := make(map[string][]byte)
		for name, data := range e.attachments {
			allFiles[name] = data
		}
		e.mu.Unlock()

		if len(allFiles) == 0 {
			return nil, fmt.Errorf("no attached files")
		}

		// If file_name is empty, upload all attachments
		if fileName == "" {
			var results []map[string]any
			for name, data := range allFiles {
				mimeType := "application/octet-stream"
				url, err := e.googleWriter.UploadFile(ctx, name, data, mimeType, folderID)
				if err != nil {
					results = append(results, map[string]any{"file_name": name, "error": err.Error()})
				} else {
					results = append(results, map[string]any{"file_name": name, "url": url})
				}
			}
			return map[string]any{"uploaded": results}, nil
		}

		// Single file upload: exact match → first available fallback
		data, ok := allFiles[fileName]
		if !ok {
			// Fallback: use first available attachment
			for name, d := range allFiles {
				fileName = name
				data = d
				ok = true
				break
			}
			if !ok {
				return nil, fmt.Errorf("no attachment found with name %q", fileName)
			}
		}

		mimeType := "application/octet-stream"
		url, err := e.googleWriter.UploadFile(ctx, fileName, data, mimeType, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "file_name": fileName}, nil

	case "delete_drive_file":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if err := e.googleWriter.DeleteFile(ctx, fileID); err != nil {
			return nil, err
		}
		return map[string]any{"deleted": fileID}, nil

	case "rename_drive_file":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		newName, _ := call.Args["new_name"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if newName == "" {
			return nil, fmt.Errorf("new_name is required")
		}
		url, err := e.googleWriter.RenameFile(ctx, fileID, newName)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "new_name": newName}, nil

	case "edit_google_doc":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		content, _ := call.Args["content"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		url, err := e.googleWriter.UpdateDocContent(ctx, fileID, content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "file_id": fileID}, nil

	case "search_drive":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		query, _ := call.Args["query"].(string)
		if query == "" {
			return nil, fmt.Errorf("query is required")
		}
		results, err := e.googleWriter.SearchDrive(ctx, query)
		if err != nil {
			return nil, err
		}
		return map[string]any{"files": results, "count": len(results)}, nil

	case "get_drive_file_info":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		info, err := e.googleWriter.GetFileInfo(ctx, fileID)
		if err != nil {
			return nil, err
		}
		return info, nil

	case "read_google_doc":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		text, err := e.googleWriter.ReadDoc(ctx, fileID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": text, "file_id": fileID}, nil

	case "read_google_sheet":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		readRange, _ := call.Args["range"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		data, err := e.googleWriter.ReadSheet(ctx, fileID, readRange)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": data, "rows": len(data), "file_id": fileID}, nil

	case "create_google_sheet":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		title, _ := call.Args["title"].(string)
		dataJSON, _ := call.Args["data"].(string)
		folderID, _ := call.Args["folder_id"].(string)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		url, err := e.googleWriter.CreateSheet(ctx, title, dataJSON, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "title": title}, nil

	case "edit_google_sheet":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		writeRange, _ := call.Args["range"].(string)
		dataJSON, _ := call.Args["data"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if writeRange == "" {
			return nil, fmt.Errorf("range is required")
		}
		if dataJSON == "" {
			return nil, fmt.Errorf("data is required")
		}
		if err := e.googleWriter.EditSheet(ctx, fileID, writeRange, dataJSON); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "range": writeRange, "status": "updated"}, nil

	case "append_google_sheet":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		appendRange, _ := call.Args["range"].(string)
		dataJSON, _ := call.Args["data"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if dataJSON == "" {
			return nil, fmt.Errorf("data is required")
		}
		if appendRange == "" {
			appendRange = "Sheet1"
		}
		if err := e.googleWriter.AppendSheet(ctx, fileID, appendRange, dataJSON); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "range": appendRange, "status": "appended"}, nil

	case "read_google_slides":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		text, err := e.googleWriter.ReadSlides(ctx, fileID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": text, "file_id": fileID}, nil

	case "create_google_slides":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		title, _ := call.Args["title"].(string)
		folderID, _ := call.Args["folder_id"].(string)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		url, err := e.googleWriter.CreateSlides(ctx, title, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "title": title}, nil

	case "add_slide":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		title, _ := call.Args["title"].(string)
		body, _ := call.Args["body"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if err := e.googleWriter.AddSlide(ctx, fileID, title, body); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "status": "slide added"}, nil

	case "move_drive_file":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		newFolderID, _ := call.Args["new_folder_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if newFolderID == "" {
			return nil, fmt.Errorf("new_folder_id is required")
		}
		url, err := e.googleWriter.MoveFile(ctx, fileID, newFolderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "file_id": fileID, "new_folder_id": newFolderID}, nil

	case "copy_drive_file":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		newName, _ := call.Args["new_name"].(string)
		folderID, _ := call.Args["folder_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		url, err := e.googleWriter.CopyFile(ctx, fileID, newName, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "file_id": fileID}, nil

	case "list_drive_folder":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		folderID, _ := call.Args["folder_id"].(string)
		if folderID == "" {
			return nil, fmt.Errorf("folder_id is required")
		}
		files, err := e.googleWriter.ListFolder(ctx, folderID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"files": files, "count": len(files)}, nil

	case "append_to_google_doc":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		content, _ := call.Args["content"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if content == "" {
			return nil, fmt.Errorf("content is required")
		}
		if err := e.googleWriter.AppendDoc(ctx, fileID, content); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "status": "appended"}, nil

	case "get_sheet_metadata":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		metadata, err := e.googleWriter.GetSheetMetadata(ctx, fileID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sheets": metadata, "count": len(metadata)}, nil

	case "clear_google_sheet":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		clearRange, _ := call.Args["range"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if clearRange == "" {
			return nil, fmt.Errorf("range is required")
		}
		if err := e.googleWriter.ClearSheet(ctx, fileID, clearRange); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "range": clearRange, "status": "cleared"}, nil

	case "share_drive_file":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		email, _ := call.Args["email"].(string)
		role, _ := call.Args["role"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if email == "" {
			return nil, fmt.Errorf("email is required")
		}
		if err := e.googleWriter.ShareFile(ctx, fileID, email, role); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "email": email, "role": role, "status": "shared"}, nil

	case "delete_slide":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		slideID, _ := call.Args["slide_id"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if err := e.googleWriter.DeleteSlide(ctx, fileID, slideID); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "status": "slide deleted"}, nil

	case "add_sheet_tab":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		tabName, _ := call.Args["tab_name"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		if tabName == "" {
			return nil, fmt.Errorf("tab_name is required")
		}
		if err := e.googleWriter.AddSheetTab(ctx, fileID, tabName); err != nil {
			return nil, err
		}
		return map[string]any{"file_id": fileID, "tab_name": tabName, "status": "tab added"}, nil

	case "export_as_pdf":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Drive is not configured")
		}
		fileID, _ := call.Args["file_id"].(string)
		fileName, _ := call.Args["file_name"].(string)
		if fileID == "" {
			return nil, fmt.Errorf("file_id is required")
		}
		url, err := e.googleWriter.ExportPDF(ctx, fileID, fileName)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "file_id": fileID}, nil

	// --- Calendar ---
	case "create_calendar_event":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		summary, _ := call.Args["summary"].(string)
		startTime, _ := call.Args["start_time"].(string)
		endTime, _ := call.Args["end_time"].(string)
		calendarID, _ := call.Args["calendar_id"].(string)
		description, _ := call.Args["description"].(string)
		location, _ := call.Args["location"].(string)
		attendeesStr, _ := call.Args["attendees"].(string)
		if summary == "" {
			return nil, fmt.Errorf("summary is required")
		}
		if startTime == "" || endTime == "" {
			return nil, fmt.Errorf("start_time and end_time are required")
		}
		var attendees []string
		if attendeesStr != "" {
			for _, a := range strings.Split(attendeesStr, ",") {
				if trimmed := strings.TrimSpace(a); trimmed != "" {
					attendees = append(attendees, trimmed)
				}
			}
		}
		url, err := e.googleWriter.CreateEvent(ctx, calendarID, summary, description, location, startTime, endTime, attendees)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "summary": summary}, nil

	case "edit_calendar_event":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		eventID, _ := call.Args["event_id"].(string)
		calendarID, _ := call.Args["calendar_id"].(string)
		summary, _ := call.Args["summary"].(string)
		description, _ := call.Args["description"].(string)
		location, _ := call.Args["location"].(string)
		startTime, _ := call.Args["start_time"].(string)
		endTime, _ := call.Args["end_time"].(string)
		attendeesStr, _ := call.Args["attendees"].(string)
		if eventID == "" {
			return nil, fmt.Errorf("event_id is required")
		}
		var attendees []string
		if attendeesStr != "" {
			for _, a := range strings.Split(attendeesStr, ",") {
				if trimmed := strings.TrimSpace(a); trimmed != "" {
					attendees = append(attendees, trimmed)
				}
			}
		}
		url, err := e.googleWriter.EditEvent(ctx, calendarID, eventID, summary, description, location, startTime, endTime, attendees)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "status": "updated"}, nil

	case "delete_calendar_event":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		eventID, _ := call.Args["event_id"].(string)
		calendarID, _ := call.Args["calendar_id"].(string)
		if eventID == "" {
			return nil, fmt.Errorf("event_id is required")
		}
		if err := e.googleWriter.DeleteEvent(ctx, calendarID, eventID); err != nil {
			return nil, err
		}
		return map[string]any{"status": "deleted", "event_id": eventID}, nil

	case "list_calendar_events":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		calendarID, _ := call.Args["calendar_id"].(string)
		timeMin, _ := call.Args["time_min"].(string)
		timeMax, _ := call.Args["time_max"].(string)
		maxResults := 20
		if v, ok := call.Args["max_results"].(float64); ok && v > 0 {
			maxResults = int(v)
		}
		events, err := e.googleWriter.ListEvents(ctx, calendarID, timeMin, timeMax, maxResults)
		if err != nil {
			return nil, err
		}
		return map[string]any{"events": events, "count": len(events)}, nil

	case "add_event_attendees":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		eventID, _ := call.Args["event_id"].(string)
		attendeesStr, _ := call.Args["attendees"].(string)
		calendarID, _ := call.Args["calendar_id"].(string)
		if eventID == "" || attendeesStr == "" {
			return nil, fmt.Errorf("event_id and attendees are required")
		}
		var attendees []string
		for _, a := range strings.Split(attendeesStr, ",") {
			if trimmed := strings.TrimSpace(a); trimmed != "" {
				attendees = append(attendees, trimmed)
			}
		}
		if err := e.googleWriter.AddAttendees(ctx, calendarID, eventID, attendees); err != nil {
			return nil, err
		}
		return map[string]any{"status": "attendees_added", "event_id": eventID, "added": len(attendees)}, nil

	case "list_calendars":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		calendars, err := e.googleWriter.ListCalendars(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"calendars": calendars, "count": len(calendars)}, nil

	// --- Tasks ---
	case "create_task":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Tasks is not configured")
		}
		title, _ := call.Args["title"].(string)
		taskListID, _ := call.Args["task_list_id"].(string)
		notes, _ := call.Args["notes"].(string)
		due, _ := call.Args["due"].(string)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		id, err := e.googleWriter.CreateTask(ctx, taskListID, title, notes, due)
		if err != nil {
			return nil, err
		}
		return map[string]any{"task_id": id, "title": title}, nil

	case "edit_task":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Tasks is not configured")
		}
		taskID, _ := call.Args["task_id"].(string)
		taskListID, _ := call.Args["task_list_id"].(string)
		title, _ := call.Args["title"].(string)
		notes, _ := call.Args["notes"].(string)
		due, _ := call.Args["due"].(string)
		status, _ := call.Args["status"].(string)
		if taskID == "" {
			return nil, fmt.Errorf("task_id is required")
		}
		if err := e.googleWriter.EditTask(ctx, taskListID, taskID, title, notes, due, status); err != nil {
			return nil, err
		}
		return map[string]any{"status": "updated", "task_id": taskID}, nil

	case "delete_task":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Tasks is not configured")
		}
		taskID, _ := call.Args["task_id"].(string)
		taskListID, _ := call.Args["task_list_id"].(string)
		if taskID == "" {
			return nil, fmt.Errorf("task_id is required")
		}
		if err := e.googleWriter.DeleteTask(ctx, taskListID, taskID); err != nil {
			return nil, err
		}
		return map[string]any{"status": "deleted", "task_id": taskID}, nil

	case "list_tasks":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Tasks is not configured")
		}
		taskListID, _ := call.Args["task_list_id"].(string)
		maxResults := 20
		if v, ok := call.Args["max_results"].(float64); ok && v > 0 {
			maxResults = int(v)
		}
		tasksList, err := e.googleWriter.ListTasks(ctx, taskListID, maxResults)
		if err != nil {
			return nil, err
		}
		return map[string]any{"tasks": tasksList, "count": len(tasksList)}, nil

	case "list_task_lists":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Tasks is not configured")
		}
		lists, err := e.googleWriter.ListTaskLists(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"task_lists": lists, "count": len(lists)}, nil

	// --- Gmail ---
	case "send_email":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Gmail is not configured")
		}
		to, _ := call.Args["to"].(string)
		subject, _ := call.Args["subject"].(string)
		body, _ := call.Args["body"].(string)
		cc, _ := call.Args["cc"].(string)
		if to == "" || subject == "" || body == "" {
			return nil, fmt.Errorf("to, subject, and body are required")
		}
		if err := e.googleWriter.SendEmail(ctx, to, cc, subject, body); err != nil {
			return nil, err
		}
		return map[string]any{"status": "sent", "to": to, "subject": subject}, nil

	case "create_notion_page":
		if e.notionWriter == nil {
			return nil, fmt.Errorf("Notion is not configured")
		}
		title, _ := call.Args["title"].(string)
		content, _ := call.Args["content"].(string)
		parentPageID, _ := call.Args["parent_page_id"].(string)
		if title == "" {
			return nil, fmt.Errorf("title is required")
		}
		url, err := e.notionWriter.CreatePage(ctx, title, content, parentPageID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "title": title}, nil

	case "update_notion_page":
		if e.notionWriter == nil {
			return nil, fmt.Errorf("Notion is not configured")
		}
		pageID, _ := call.Args["page_id"].(string)
		content, _ := call.Args["content"].(string)
		if pageID == "" {
			return nil, fmt.Errorf("page_id is required")
		}
		url, err := e.notionWriter.AppendToPage(ctx, pageID, content)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": url, "page_id": pageID}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", call.Name)
	}
}
