package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

// ToolExecutor manages available tools and dispatches function calls.
type ToolExecutor struct {
	googleWriter      *GoogleWriter
	notionWriter      *NotionWriter
	screenshotter     *Screenshotter
	logger            *slog.Logger
	defaultCalendarID string
	schedulerManager  *SchedulerManager
	store             kb.Storage

	mu               sync.Mutex
	attachments      map[string][]byte // file name → data, from Slack file downloads
	screenshotData   []byte            // screenshot from screenshot_url tool, separate from user attachments
	attachmentEpoch  int               // incremented by SetAttachments to detect stale state
	lastSeenEpoch    int               // epoch seen by most recent Execute; detects panics between calls
}

// NewToolExecutor creates a ToolExecutor.
// Either writer can be nil if not configured. store can be nil if search is not needed.
func NewToolExecutor(gw *GoogleWriter, nw *NotionWriter, defaultCalendarID string, ss *Screenshotter, sm *SchedulerManager, store kb.Storage, logger *slog.Logger) *ToolExecutor {
	return &ToolExecutor{
		googleWriter:      gw,
		notionWriter:      nw,
		screenshotter:     ss,
		defaultCalendarID: defaultCalendarID,
		schedulerManager:  sm,
		store:             store,
		logger:            logger.With("component", "tool-executor"),
		attachments:       make(map[string][]byte),
	}
}

// SetAttachments stores file attachments from the current Slack message
// for use by upload_to_drive.
func (e *ToolExecutor) SetAttachments(attachments map[string][]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachments = attachments
	e.attachmentEpoch++
}

// ClearAttachments removes stored attachments after processing.
func (e *ToolExecutor) ClearAttachments() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachments = make(map[string][]byte)
	e.lastSeenEpoch = 0
}

// PopScreenshot removes and returns the screenshot data if present.
func (e *ToolExecutor) PopScreenshot() ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.screenshotData != nil {
		data := e.screenshotData
		e.screenshotData = nil
		return data, true
	}
	return nil, false
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
				Description: "Write data to a specific range in Google Sheets (overwrites existing data). IMPORTANT: Always read_google_sheet first to check the column_map (e.g. A=Name, B=Email) before writing, so you target the correct columns.",
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
							"description": "Start time in RFC3339 format (YYYY-MM-DDTHH:MM:SS+09:00 for KST). Use date-only for all-day events (YYYY-MM-DD)",
						},
						"end_time": map[string]any{
							"type":        "string",
							"description": "End time in RFC3339 format (YYYY-MM-DDTHH:MM:SS+09:00 for KST). Use next day for all-day events (YYYY-MM-DD)",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "Calendar ID (uses the team shared calendar by default — only specify to override)",
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
						"google_meet": map[string]any{
							"type":        "boolean",
							"description": "Set to true to create a Google Meet video conference link (default: false)",
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
							"description": "Calendar ID (uses the team shared calendar by default — only specify to override)",
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
							"description": "Calendar ID (uses the team shared calendar by default — only specify to override)",
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
							"description": "Calendar ID (uses the team shared calendar by default — only specify to override)",
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
							"description": "Calendar ID (uses the team shared calendar by default — only specify to override)",
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
				Description: "Send an email to internal team members only (@xylolabs.com). External addresses are blocked. Use for work-related emails, invitations, and notifications.",
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

	// --- Screenshot Tool ---
	if e.screenshotter != nil {
		decls = append(decls,
			gemini.FunctionDeclaration{
				Name:        "screenshot_url",
				Description: "Take a screenshot of a web page and send it as an image. Use when the user wants to see what a web page looks like.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "URL of the web page to capture",
						},
						"full_page": map[string]any{
							"type":        "boolean",
							"description": "Capture full scrollable page (default: false, captures only viewport)",
						},
					},
					"required": []string{"url"},
				},
			},
		)
	}

	// --- Scheduler Tools ---
	if e.schedulerManager != nil {
		decls = append(decls,
			gemini.FunctionDeclaration{
				Name:        "schedule_message",
				Description: "Schedule a one-time message to be sent to a Slack channel at a specific time. Use for reminders and delayed messages.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"channel": map[string]any{
							"type":        "string",
							"description": "Channel name (e.g. #general) or channel ID",
						},
						"message": map[string]any{
							"type":        "string",
							"description": "Message text to send",
						},
						"send_at": map[string]any{
							"type":        "string",
							"description": "When to send (RFC3339 e.g. 2025-03-11T15:00:00+09:00, or relative e.g. 'in 30 minutes', 'in 2 hours')",
						},
					},
					"required": []string{"channel", "message", "send_at"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_recurring_job",
				Description: "Create a recurring scheduled job that sends a message to a Slack channel on a cron schedule. Use for daily standups, weekly reminders, etc.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"channel": map[string]any{
							"type":        "string",
							"description": "Channel name (e.g. #general) or channel ID",
						},
						"message": map[string]any{
							"type":        "string",
							"description": "Message text to send each time",
						},
						"cron_expression": map[string]any{
							"type":        "string",
							"description": "Standard 5-field cron expression (minute hour day-of-month month day-of-week). Examples: '0 9 * * 1-5' (weekdays 9am), '0 9 * * *' (daily 9am), '*/30 * * * *' (every 30 min)",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "Human-readable description of the job (optional)",
						},
					},
					"required": []string{"channel", "message", "cron_expression"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_scheduled_jobs",
				Description: "List all active scheduled jobs (one-time and recurring).",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "cancel_scheduled_job",
				Description: "Cancel a scheduled job by its ID.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job_id": map[string]any{
							"type":        "string",
							"description": "ID of the scheduled job to cancel",
						},
					},
					"required": []string{"job_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "send_message",
				Description: "Send a message to a Slack channel immediately. Use when asked to post, share, or send something to a specific channel.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"channel": map[string]any{
							"type":        "string",
							"description": "Channel name (e.g. #general) or channel ID",
						},
						"message": map[string]any{
							"type":        "string",
							"description": "Message text (supports Slack mrkdwn formatting)",
						},
						"thread_ts": map[string]any{
							"type":        "string",
							"description": "Thread timestamp to reply in a specific thread (optional)",
						},
					},
					"required": []string{"channel", "message"},
				},
			},
		)
	}

	// --- Knowledge Base Search Tool ---
	if e.store != nil {
		decls = append(decls,
			gemini.FunctionDeclaration{
				Name:        "search_knowledge_base",
				Description: "Search the knowledge base for messages, documents, and conversations using full-text search. Use when the user asks to find specific keywords, messages, or content across Slack, Google, and Notion.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "Search keywords",
						},
						"source": map[string]any{
							"type":        "string",
							"description": "Filter by source (slack, google, notion, discord)",
						},
						"channel": map[string]any{
							"type":        "string",
							"description": "Filter by channel name",
						},
						"author": map[string]any{
							"type":        "string",
							"description": "Filter by author name",
						},
						"limit": map[string]any{
							"type":        "number",
							"description": "Max results (default 10, max 50)",
						},
					},
					"required": []string{"query"},
				},
			},
		)
	}

	return decls
}

// Execute runs a function call and returns the result.
func (e *ToolExecutor) Execute(ctx context.Context, call gemini.FunctionCall) gemini.FunctionResponse {
	// Safety: detect stale state from a previous Execute that panicked
	// before the bot handler could ClearAttachments. Only clear if the
	// epoch doesn't match (i.e., attachments are from a prior session).
	e.mu.Lock()
	if e.lastSeenEpoch > 0 && e.lastSeenEpoch < e.attachmentEpoch {
		e.logger.Warn("stale attachments from previous session detected, clearing",
			"last_seen_epoch", e.lastSeenEpoch, "current_epoch", e.attachmentEpoch)
		e.attachments = nil
		e.screenshotData = nil
	}
	e.lastSeenEpoch = e.attachmentEpoch
	e.mu.Unlock()

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

		// Single file upload: exact match only
		data, ok := allFiles[fileName]
		if !ok {
			available := make([]string, 0, len(allFiles))
			for name := range allFiles {
				available = append(available, name)
			}
			sort.Strings(available)
			return nil, fmt.Errorf("no attachment found with name %q. Available files: %s", fileName, strings.Join(available, ", "))
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

		result := map[string]any{"data": data, "rows": len(data), "file_id": fileID}

		if len(data) > 0 {
			startCol := parseStartColumn(readRange)
			startIdx := columnIndex(startCol)

			// Build "A=Header1, B=Header2, ..." mapping from the first row
			var colParts []string
			for i, header := range data[0] {
				letter := columnLetter(startIdx + i)
				colParts = append(colParts, letter+"="+header)
			}
			result["column_map"] = strings.Join(colParts, ", ")
			result["columns"] = len(data[0])
		}

		return result, nil

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
		if calendarID == "" {
			calendarID = e.defaultCalendarID
		}
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
		addMeet, _ := call.Args["google_meet"].(bool)
		url, meetLink, err := e.googleWriter.CreateEvent(ctx, calendarID, summary, description, location, startTime, endTime, attendees, addMeet)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"url": url, "summary": summary}
		if meetLink != "" {
			result["meet_link"] = meetLink
		}
		return result, nil

	case "edit_calendar_event":
		if e.googleWriter == nil {
			return nil, fmt.Errorf("Google Calendar is not configured")
		}
		eventID, _ := call.Args["event_id"].(string)
		calendarID, _ := call.Args["calendar_id"].(string)
		if calendarID == "" {
			calendarID = e.defaultCalendarID
		}
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
		if calendarID == "" {
			calendarID = e.defaultCalendarID
		}
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
		if calendarID == "" {
			calendarID = e.defaultCalendarID
		}
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
		if calendarID == "" {
			calendarID = e.defaultCalendarID
		}
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
		if e.googleWriter.SenderEmail() == "" {
			return nil, fmt.Errorf("send_email is not configured: sender email is not set")
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

	case "screenshot_url":
		if e.screenshotter == nil {
			return nil, fmt.Errorf("screenshot is not configured")
		}
		url, _ := call.Args["url"].(string)
		if url == "" {
			return nil, fmt.Errorf("url is required")
		}
		fullPage, _ := call.Args["full_page"].(bool)

		pngBytes, err := e.screenshotter.Capture(ctx, url, 1280, 960, fullPage)
		if err != nil {
			return nil, err
		}

		e.mu.Lock()
		e.screenshotData = pngBytes
		e.mu.Unlock()

		return map[string]any{
			"status":     "captured",
			"file_name":  "screenshot.png",
			"size_bytes": len(pngBytes),
			"message":    "Screenshot captured. The image has been attached and will be sent with the response.",
		}, nil

	// --- Scheduler ---
	case "schedule_message":
		if e.schedulerManager == nil {
			return nil, fmt.Errorf("scheduler is not configured")
		}
		channel, _ := call.Args["channel"].(string)
		message, _ := call.Args["message"].(string)
		sendAt, _ := call.Args["send_at"].(string)
		if channel == "" || message == "" || sendAt == "" {
			return nil, fmt.Errorf("channel, message, and send_at are required")
		}
		return e.schedulerManager.ScheduleMessage(channel, message, sendAt, "")

	case "create_recurring_job":
		if e.schedulerManager == nil {
			return nil, fmt.Errorf("scheduler is not configured")
		}
		channel, _ := call.Args["channel"].(string)
		message, _ := call.Args["message"].(string)
		cronExpr, _ := call.Args["cron_expression"].(string)
		description, _ := call.Args["description"].(string)
		if channel == "" || message == "" || cronExpr == "" {
			return nil, fmt.Errorf("channel, message, and cron_expression are required")
		}
		return e.schedulerManager.CreateRecurringJob(channel, message, cronExpr, description, "")

	case "list_scheduled_jobs":
		if e.schedulerManager == nil {
			return nil, fmt.Errorf("scheduler is not configured")
		}
		return e.schedulerManager.ListJobs()

	case "cancel_scheduled_job":
		if e.schedulerManager == nil {
			return nil, fmt.Errorf("scheduler is not configured")
		}
		jobID, _ := call.Args["job_id"].(string)
		if jobID == "" {
			return nil, fmt.Errorf("job_id is required")
		}
		return e.schedulerManager.CancelJob(jobID)

	case "send_message":
		if e.schedulerManager == nil {
			return nil, fmt.Errorf("scheduler is not configured")
		}
		channel, _ := call.Args["channel"].(string)
		message, _ := call.Args["message"].(string)
		threadTS, _ := call.Args["thread_ts"].(string)
		if channel == "" || message == "" {
			return nil, fmt.Errorf("channel and message are required")
		}
		return e.schedulerManager.SendMessage(channel, message, threadTS)

	// --- Knowledge Base Search ---
	case "search_knowledge_base":
		if e.store == nil {
			return nil, fmt.Errorf("knowledge base search is not configured")
		}
		query, _ := call.Args["query"].(string)
		if query == "" {
			return nil, fmt.Errorf("query is required")
		}
		source, _ := call.Args["source"].(string)
		channel, _ := call.Args["channel"].(string)
		author, _ := call.Args["author"].(string)
		limit := 10
		if v, ok := call.Args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if limit > 50 {
			limit = 50
		}

		sq := kb.SearchQuery{
			Query:   query,
			Source:  kb.Source(source),
			Channel: channel,
			Author:  author,
			Limit:   limit,
		}
		results, err := e.store.Search(sq)
		if err != nil {
			return nil, fmt.Errorf("search failed: %w", err)
		}

		var items []map[string]any
		for _, r := range results.Results {
			item := map[string]any{
				"author":    r.Document.Author,
				"channel":   r.Document.Channel,
				"source":    string(r.Document.Source),
				"timestamp": r.Document.Timestamp.Format("2006-01-02 15:04"),
				"snippet":   r.Snippet,
				"score":     r.Score,
			}
			if r.Document.Title != "" {
				item["title"] = r.Document.Title
			}
			if r.Document.URL != "" {
				item["url"] = r.Document.URL
			}
			items = append(items, item)
		}

		return map[string]any{"results": items, "count": len(items), "query": query}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", call.Name)
	}
}

// columnLetter converts a 0-based column index to a spreadsheet column letter.
// 0→A, 1→B, ..., 25→Z, 26→AA, 27→AB, ...
func columnLetter(idx int) string {
	var result string
	for idx >= 0 {
		result = string(rune('A'+idx%26)) + result
		idx = idx/26 - 1
	}
	return result
}

// parseStartColumn extracts the starting column letter from a range string.
// "Sheet1" → "A", "Sheet1!C3:F10" → "C", "B2:D5" → "B"
func parseStartColumn(rangeStr string) string {
	// Remove sheet name prefix
	if idx := strings.Index(rangeStr, "!"); idx >= 0 {
		rangeStr = rangeStr[idx+1:]
	}
	// Extract leading letters (column part)
	var col string
	for _, ch := range rangeStr {
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' {
			col += string(ch)
		} else {
			break
		}
	}
	if col == "" {
		return "A"
	}
	return strings.ToUpper(col)
}

// columnIndex converts a column letter to a 0-based index. A→0, B→1, ..., Z→25, AA→26
func columnIndex(col string) int {
	col = strings.ToUpper(col)
	idx := 0
	for _, ch := range col {
		idx = idx*26 + int(ch-'A') + 1
	}
	return idx - 1
}
