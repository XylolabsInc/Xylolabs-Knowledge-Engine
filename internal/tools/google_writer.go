package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/docs/v1"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/slides/v1"
	"google.golang.org/api/tasks/v1"
)

const defaultDriveFolderID = ""

// GoogleWriter handles Google Workspace write/read operations.
type GoogleWriter struct {
	service         *drive.Service
	docsService     *docs.Service
	sheetsService   *sheets.Service
	slidesService   *slides.Service
	calendarService *calendar.Service
	tasksService    *tasks.Service
	gmailService    *gmail.Service
	senderEmail     string
	logger          *slog.Logger
}

// NewGoogleWriter creates a GoogleWriter from Google API services.
func NewGoogleWriter(driveService *drive.Service, docsService *docs.Service, sheetsService *sheets.Service, slidesService *slides.Service, calendarService *calendar.Service, tasksService *tasks.Service, gmailService *gmail.Service, senderEmail string, logger *slog.Logger) *GoogleWriter {
	return &GoogleWriter{
		service:         driveService,
		docsService:     docsService,
		sheetsService:   sheetsService,
		slidesService:   slidesService,
		calendarService: calendarService,
		tasksService:    tasksService,
		gmailService:    gmailService,
		senderEmail:     senderEmail,
		logger:          logger.With("component", "google-writer"),
	}
}

// CreateDoc creates a new Google Doc with the given title and content.
// Content is uploaded as text/plain and auto-converted to Google Docs format.
// If folderID is empty, uses the default shared drive folder.
// Returns the URL of the created document.
func (w *GoogleWriter) CreateDoc(ctx context.Context, title, content, folderID string) (string, error) {
	if folderID == "" {
		folderID = defaultDriveFolderID
	}

	file := &drive.File{
		Name:     title,
		MimeType: "application/vnd.google-apps.document",
		Parents:  []string{folderID},
	}

	htmlContent := markdownToHTML(content)
	created, err := w.service.Files.Create(file).
		Context(ctx).
		Media(bytes.NewReader([]byte(htmlContent)), googleapi.ContentType("text/html")).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("create google doc: %w", err)
	}

	w.logger.Info("created google doc", "title", title, "id", created.Id, "url", created.WebViewLink)
	return created.WebViewLink, nil
}

// CreateFolder creates a new folder in Google Drive.
// If parentFolderID is empty, uses the default shared drive folder.
// Returns the folder ID and URL.
func (w *GoogleWriter) CreateFolder(ctx context.Context, name, parentFolderID string) (folderID, webViewLink string, err error) {
	if parentFolderID == "" {
		parentFolderID = defaultDriveFolderID
	}

	file := &drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentFolderID},
	}

	created, err := w.service.Files.Create(file).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", "", fmt.Errorf("create drive folder: %w", err)
	}

	w.logger.Info("created drive folder", "name", name, "id", created.Id, "url", created.WebViewLink)
	return created.Id, created.WebViewLink, nil
}

// UploadFile uploads a file to Google Drive.
// If folderID is empty, uses the default shared drive folder.
// Returns the URL of the uploaded file.
func (w *GoogleWriter) UploadFile(ctx context.Context, fileName string, data []byte, mimeType, folderID string) (string, error) {
	if folderID == "" {
		folderID = defaultDriveFolderID
	}

	file := &drive.File{
		Name:    fileName,
		Parents: []string{folderID},
	}

	created, err := w.service.Files.Create(file).
		Context(ctx).
		Media(bytes.NewReader(data)).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("upload to drive: %w", err)
	}

	w.logger.Info("uploaded file to drive", "name", fileName, "id", created.Id, "url", created.WebViewLink)
	return created.WebViewLink, nil
}

// DeleteFile deletes a file or folder from Google Drive.
func (w *GoogleWriter) DeleteFile(ctx context.Context, fileID string) error {
	err := w.service.Files.Delete(fileID).
		Context(ctx).
		SupportsAllDrives(true).
		Do()
	if err != nil {
		return fmt.Errorf("delete drive file: %w", err)
	}
	w.logger.Info("deleted drive file", "id", fileID)
	return nil
}

// RenameFile renames a file or folder in Google Drive.
func (w *GoogleWriter) RenameFile(ctx context.Context, fileID, newName string) (string, error) {
	updated, err := w.service.Files.Update(fileID, &drive.File{Name: newName}).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, name, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("rename drive file: %w", err)
	}
	w.logger.Info("renamed drive file", "id", fileID, "new_name", updated.Name, "url", updated.WebViewLink)
	return updated.WebViewLink, nil
}

var (
	reOrderedList  = regexp.MustCompile(`^\d+\.\s+`)
	reMdBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMdInlineCode = regexp.MustCompile("`([^`]+)`")
	reMdLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// formatInline converts inline Markdown formatting to HTML.
func formatInline(text string) string {
	text = reMdLink.ReplaceAllString(text, `<a href="$2">$1</a>`)
	text = reMdBold.ReplaceAllString(text, "<strong>$1</strong>")
	text = reMdInlineCode.ReplaceAllString(text, "<code>$1</code>")
	return text
}

// markdownToHTML converts basic Markdown to HTML for Google Docs import.
func markdownToHTML(md string) string {
	var sb strings.Builder
	sb.WriteString("<html><body>")

	lines := strings.Split(md, "\n")
	inCodeBlock := false
	inList := false
	listType := "" // "ul" or "ol"

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// Code blocks
		if strings.HasPrefix(line, "```") {
			if inCodeBlock {
				sb.WriteString("</code></pre>")
				inCodeBlock = false
			} else {
				if inList {
					sb.WriteString(fmt.Sprintf("</%s>", listType))
					inList = false
				}
				sb.WriteString("<pre><code>")
				inCodeBlock = true
			}
			continue
		}
		if inCodeBlock {
			sb.WriteString(strings.ReplaceAll(line, "<", "&lt;"))
			sb.WriteString("\n")
			continue
		}

		// Close list if current line is not a list item
		isUnordered := strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")
		isOrdered := reOrderedList.MatchString(line)
		if inList && !isUnordered && !isOrdered {
			sb.WriteString(fmt.Sprintf("</%s>", listType))
			inList = false
		}

		// Empty line
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Headings
		if strings.HasPrefix(line, "# ") {
			sb.WriteString(fmt.Sprintf("<h1>%s</h1>", formatInline(strings.TrimPrefix(line, "# "))))
			continue
		}
		if strings.HasPrefix(line, "## ") {
			sb.WriteString(fmt.Sprintf("<h2>%s</h2>", formatInline(strings.TrimPrefix(line, "## "))))
			continue
		}
		if strings.HasPrefix(line, "### ") {
			sb.WriteString(fmt.Sprintf("<h3>%s</h3>", formatInline(strings.TrimPrefix(line, "### "))))
			continue
		}
		if strings.HasPrefix(line, "#### ") {
			sb.WriteString(fmt.Sprintf("<h4>%s</h4>", formatInline(strings.TrimPrefix(line, "#### "))))
			continue
		}

		// Unordered list
		if isUnordered {
			if !inList || listType != "ul" {
				if inList {
					sb.WriteString(fmt.Sprintf("</%s>", listType))
				}
				sb.WriteString("<ul>")
				inList = true
				listType = "ul"
			}
			item := strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* ")
			sb.WriteString(fmt.Sprintf("<li>%s</li>", formatInline(item)))
			continue
		}

		// Ordered list
		if isOrdered {
			if !inList || listType != "ol" {
				if inList {
					sb.WriteString(fmt.Sprintf("</%s>", listType))
				}
				sb.WriteString("<ol>")
				inList = true
				listType = "ol"
			}
			item := reOrderedList.ReplaceAllString(line, "")
			sb.WriteString(fmt.Sprintf("<li>%s</li>", formatInline(item)))
			continue
		}

		// Regular paragraph
		sb.WriteString(fmt.Sprintf("<p>%s</p>", formatInline(line)))
	}

	if inList {
		sb.WriteString(fmt.Sprintf("</%s>", listType))
	}
	if inCodeBlock {
		sb.WriteString("</code></pre>")
	}

	sb.WriteString("</body></html>")
	return sb.String()
}

// SearchDrive searches files by name/query in Google Drive.
func (w *GoogleWriter) SearchDrive(ctx context.Context, query string) ([]map[string]any, error) {
	q := fmt.Sprintf("name contains '%s' and trashed = false", strings.ReplaceAll(query, "'", "\\'"))
	fileList, err := w.service.Files.List().
		Context(ctx).
		Q(q).
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Corpora("allDrives").
		Fields("files(id, name, mimeType, webViewLink, modifiedTime)").
		PageSize(20).
		Do()
	if err != nil {
		return nil, fmt.Errorf("search drive: %w", err)
	}

	var results []map[string]any
	for _, f := range fileList.Files {
		results = append(results, map[string]any{
			"id":           f.Id,
			"name":         f.Name,
			"mime_type":    f.MimeType,
			"url":          f.WebViewLink,
			"modified_time": f.ModifiedTime,
		})
	}
	w.logger.Info("searched drive", "query", query, "results", len(results))
	return results, nil
}

// GetFileInfo returns metadata for a Google Drive file.
func (w *GoogleWriter) GetFileInfo(ctx context.Context, fileID string) (map[string]any, error) {
	f, err := w.service.Files.Get(fileID).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, name, mimeType, webViewLink, modifiedTime, size, parents").
		Do()
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}

	return map[string]any{
		"id":            f.Id,
		"name":          f.Name,
		"mime_type":     f.MimeType,
		"url":           f.WebViewLink,
		"modified_time": f.ModifiedTime,
		"size":          f.Size,
		"parents":       f.Parents,
	}, nil
}

// ReadDoc reads Google Docs content as plain text.
func (w *GoogleWriter) ReadDoc(ctx context.Context, fileID string) (string, error) {
	doc, err := w.docsService.Documents.Get(fileID).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("read google doc: %w", err)
	}

	var sb strings.Builder
	if doc.Body != nil {
		for _, elem := range doc.Body.Content {
			if elem.Paragraph != nil {
				for _, pe := range elem.Paragraph.Elements {
					if pe.TextRun != nil {
						sb.WriteString(pe.TextRun.Content)
					}
				}
			}
			if elem.Table != nil {
				for _, row := range elem.Table.TableRows {
					for ci, cell := range row.TableCells {
						if ci > 0 {
							sb.WriteString("\t")
						}
						for _, cellContent := range cell.Content {
							if cellContent.Paragraph != nil {
								for _, pe := range cellContent.Paragraph.Elements {
									if pe.TextRun != nil {
										sb.WriteString(strings.TrimRight(pe.TextRun.Content, "\n"))
									}
								}
							}
						}
					}
					sb.WriteString("\n")
				}
			}
		}
	}

	w.logger.Info("read google doc", "id", fileID, "title", doc.Title)
	return sb.String(), nil
}

// ReadSheet reads Google Sheets data as a 2D string array.
func (w *GoogleWriter) ReadSheet(ctx context.Context, fileID, readRange string) ([][]string, error) {
	if readRange == "" {
		readRange = "Sheet1"
	}
	resp, err := w.sheetsService.Spreadsheets.Values.Get(fileID, readRange).
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("read google sheet: %w", err)
	}

	var result [][]string
	for _, row := range resp.Values {
		var strRow []string
		for _, cell := range row {
			strRow = append(strRow, fmt.Sprintf("%v", cell))
		}
		result = append(result, strRow)
	}

	w.logger.Info("read google sheet", "id", fileID, "range", readRange, "rows", len(result))
	return result, nil
}

// CreateSheet creates a new Google Spreadsheet with optional initial data.
func (w *GoogleWriter) CreateSheet(ctx context.Context, title, dataJSON, folderID string) (string, error) {
	if folderID == "" {
		folderID = defaultDriveFolderID
	}

	file := &drive.File{
		Name:     title,
		MimeType: "application/vnd.google-apps.spreadsheet",
		Parents:  []string{folderID},
	}

	created, err := w.service.Files.Create(file).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("create spreadsheet: %w", err)
	}

	// Write initial data if provided
	if dataJSON != "" {
		var data [][]interface{}
		if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
			w.logger.Warn("failed to parse sheet data JSON", "error", err)
		} else if len(data) > 0 {
			vr := &sheets.ValueRange{Values: data}
			_, err := w.sheetsService.Spreadsheets.Values.Update(created.Id, "Sheet1", vr).
				Context(ctx).
				ValueInputOption("USER_ENTERED").
				Do()
			if err != nil {
				w.logger.Warn("failed to write initial data", "error", err)
			}
		}
	}

	w.logger.Info("created spreadsheet", "title", title, "id", created.Id, "url", created.WebViewLink)
	return created.WebViewLink, nil
}

// EditSheet writes data to a specific range in a Google Sheet.
func (w *GoogleWriter) EditSheet(ctx context.Context, fileID, writeRange, dataJSON string) error {
	var data [][]interface{}
	if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
		return fmt.Errorf("parse data JSON: %w", err)
	}

	vr := &sheets.ValueRange{Values: data}
	_, err := w.sheetsService.Spreadsheets.Values.Update(fileID, writeRange, vr).
		Context(ctx).
		ValueInputOption("USER_ENTERED").
		Do()
	if err != nil {
		return fmt.Errorf("edit sheet: %w", err)
	}

	w.logger.Info("edited sheet", "id", fileID, "range", writeRange)
	return nil
}

// AppendSheet appends rows to a Google Sheet.
func (w *GoogleWriter) AppendSheet(ctx context.Context, fileID, appendRange, dataJSON string) error {
	var data [][]interface{}
	if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
		return fmt.Errorf("parse data JSON: %w", err)
	}

	vr := &sheets.ValueRange{Values: data}
	_, err := w.sheetsService.Spreadsheets.Values.Append(fileID, appendRange, vr).
		Context(ctx).
		ValueInputOption("USER_ENTERED").
		InsertDataOption("INSERT_ROWS").
		Do()
	if err != nil {
		return fmt.Errorf("append sheet: %w", err)
	}

	w.logger.Info("appended to sheet", "id", fileID, "range", appendRange)
	return nil
}

// ReadSlides reads Google Slides content as text.
func (w *GoogleWriter) ReadSlides(ctx context.Context, fileID string) (string, error) {
	pres, err := w.slidesService.Presentations.Get(fileID).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("read slides: %w", err)
	}

	var sb strings.Builder
	for i, slide := range pres.Slides {
		sb.WriteString(fmt.Sprintf("--- Slide %d ---\n", i+1))
		for _, elem := range slide.PageElements {
			if elem.Shape != nil && elem.Shape.Text != nil {
				for _, te := range elem.Shape.Text.TextElements {
					if te.TextRun != nil {
						sb.WriteString(te.TextRun.Content)
					}
				}
			}
		}
		sb.WriteString("\n")
	}

	w.logger.Info("read slides", "id", fileID, "title", pres.Title, "slides", len(pres.Slides))
	return sb.String(), nil
}

// CreateSlides creates a new Google Slides presentation.
// It populates the title text on the default first slide.
func (w *GoogleWriter) CreateSlides(ctx context.Context, title, folderID string) (string, error) {
	if folderID == "" {
		folderID = defaultDriveFolderID
	}

	file := &drive.File{
		Name:     title,
		MimeType: "application/vnd.google-apps.presentation",
		Parents:  []string{folderID},
	}

	created, err := w.service.Files.Create(file).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("create presentation: %w", err)
	}

	// Populate title on the default first slide
	if title != "" {
		pres, err := w.slidesService.Presentations.Get(created.Id).Context(ctx).Do()
		if err == nil && len(pres.Slides) > 0 {
			for _, elem := range pres.Slides[0].PageElements {
				if elem.Shape != nil && elem.Shape.Placeholder != nil && elem.Shape.Placeholder.Type == "CENTERED_TITLE" {
					_, _ = w.slidesService.Presentations.BatchUpdate(created.Id, &slides.BatchUpdatePresentationRequest{
						Requests: []*slides.Request{
							{InsertText: &slides.InsertTextRequest{
								ObjectId: elem.ObjectId,
								Text:     title,
							}},
						},
					}).Context(ctx).Do()
					break
				}
			}
		}
	}

	w.logger.Info("created presentation", "title", title, "id", created.Id, "url", created.WebViewLink)
	return created.WebViewLink, nil
}

// AddSlide adds a new slide with title and body text to a presentation.
func (w *GoogleWriter) AddSlide(ctx context.Context, fileID, title, body string) error {
	slideID := fmt.Sprintf("slide_%d", time.Now().UnixNano())
	titleID := slideID + "_title"
	bodyID := slideID + "_body"

	requests := []*slides.Request{
		{
			CreateSlide: &slides.CreateSlideRequest{
				ObjectId: slideID,
				SlideLayoutReference: &slides.LayoutReference{
					PredefinedLayout: "TITLE_AND_BODY",
				},
				PlaceholderIdMappings: []*slides.LayoutPlaceholderIdMapping{
					{
						LayoutPlaceholder: &slides.Placeholder{Type: "TITLE"},
						ObjectId:          titleID,
					},
					{
						LayoutPlaceholder: &slides.Placeholder{Type: "BODY"},
						ObjectId:          bodyID,
					},
				},
			},
		},
	}

	if title != "" {
		requests = append(requests, &slides.Request{
			InsertText: &slides.InsertTextRequest{
				ObjectId: titleID,
				Text:     title,
			},
		})
	}

	if body != "" {
		requests = append(requests, &slides.Request{
			InsertText: &slides.InsertTextRequest{
				ObjectId: bodyID,
				Text:     body,
			},
		})
	}

	_, err := w.slidesService.Presentations.BatchUpdate(fileID, &slides.BatchUpdatePresentationRequest{
		Requests: requests,
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("add slide: %w", err)
	}

	w.logger.Info("added slide", "presentation_id", fileID, "slide_id", slideID)
	return nil
}

// UpdateDocContent replaces the content of an existing Google Doc.
func (w *GoogleWriter) UpdateDocContent(ctx context.Context, fileID, content string) (string, error) {
	htmlContent := markdownToHTML(content)
	updated, err := w.service.Files.Update(fileID, &drive.File{}).
		Context(ctx).
		Media(bytes.NewReader([]byte(htmlContent)), googleapi.ContentType("text/html")).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("update doc content: %w", err)
	}
	w.logger.Info("updated doc content", "id", fileID, "url", updated.WebViewLink)
	return updated.WebViewLink, nil
}

// MoveFile moves a file to a different folder in Google Drive.
// Returns the webViewLink of the moved file.
func (w *GoogleWriter) MoveFile(ctx context.Context, fileID, newFolderID string) (string, error) {
	f, err := w.service.Files.Get(fileID).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("parents").
		Do()
	if err != nil {
		return "", fmt.Errorf("get file parents: %w", err)
	}

	currentParents := strings.Join(f.Parents, ",")
	updated, err := w.service.Files.Update(fileID, &drive.File{}).
		Context(ctx).
		AddParents(newFolderID).
		RemoveParents(currentParents).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("move drive file: %w", err)
	}

	w.logger.Info("moved drive file", "id", fileID, "new_folder", newFolderID, "url", updated.WebViewLink)
	return updated.WebViewLink, nil
}

// CopyFile copies/duplicates a file in Google Drive.
// If folderID is empty, uses the default shared drive folder.
// If newName is empty, Drive will default to "Copy of <original name>".
// Returns the webViewLink of the copy.
func (w *GoogleWriter) CopyFile(ctx context.Context, fileID, newName, folderID string) (string, error) {
	if folderID == "" {
		folderID = defaultDriveFolderID
	}

	copyFile := &drive.File{
		Parents: []string{folderID},
	}
	if newName != "" {
		copyFile.Name = newName
	}

	copied, err := w.service.Files.Copy(fileID, copyFile).
		Context(ctx).
		SupportsAllDrives(true).
		Fields("id, webViewLink").
		Do()
	if err != nil {
		return "", fmt.Errorf("copy drive file: %w", err)
	}

	w.logger.Info("copied drive file", "source_id", fileID, "copy_id", copied.Id, "url", copied.WebViewLink)
	return copied.WebViewLink, nil
}

// ListFolder lists files in a Google Drive folder.
// Returns a slice of maps with id, name, mime_type, url, modified_time.
func (w *GoogleWriter) ListFolder(ctx context.Context, folderID string) ([]map[string]any, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", strings.ReplaceAll(folderID, "'", "\\'"))
	fileList, err := w.service.Files.List().
		Context(ctx).
		Q(q).
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Corpora("allDrives").
		Fields("files(id, name, mimeType, webViewLink, modifiedTime)").
		PageSize(100).
		Do()
	if err != nil {
		return nil, fmt.Errorf("list folder: %w", err)
	}

	var results []map[string]any
	for _, f := range fileList.Files {
		results = append(results, map[string]any{
			"id":            f.Id,
			"name":          f.Name,
			"mime_type":     f.MimeType,
			"url":           f.WebViewLink,
			"modified_time": f.ModifiedTime,
		})
	}

	w.logger.Info("listed folder", "folder_id", folderID, "count", len(results))
	return results, nil
}

// AppendDoc appends content to an existing Google Doc.
func (w *GoogleWriter) AppendDoc(ctx context.Context, fileID, content string) error {
	doc, err := w.docsService.Documents.Get(fileID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("get google doc: %w", err)
	}

	var endIndex int64
	if doc.Body != nil && len(doc.Body.Content) > 0 {
		lastElem := doc.Body.Content[len(doc.Body.Content)-1]
		endIndex = lastElem.EndIndex - 1
	}

	req := &docs.BatchUpdateDocumentRequest{
		Requests: []*docs.Request{
			{
				InsertText: &docs.InsertTextRequest{
					Location: &docs.Location{Index: endIndex},
					Text:     "\n" + content,
				},
			},
		},
	}

	_, err = w.docsService.Documents.BatchUpdate(fileID, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("append to google doc: %w", err)
	}

	w.logger.Info("appended to google doc", "id", fileID)
	return nil
}

// GetSheetMetadata returns spreadsheet metadata including sheet tab names and dimensions.
// Returns a slice of maps with title, sheet_id, row_count, column_count.
func (w *GoogleWriter) GetSheetMetadata(ctx context.Context, fileID string) ([]map[string]any, error) {
	resp, err := w.sheetsService.Spreadsheets.Get(fileID).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get spreadsheet metadata: %w", err)
	}

	var results []map[string]any
	for _, s := range resp.Sheets {
		props := s.Properties
		result := map[string]any{
			"title":    props.Title,
			"sheet_id": props.SheetId,
		}
		if props.GridProperties != nil {
			result["row_count"] = props.GridProperties.RowCount
			result["column_count"] = props.GridProperties.ColumnCount
		}
		results = append(results, result)
	}

	w.logger.Info("got sheet metadata", "id", fileID, "sheets", len(results))
	return results, nil
}

// ClearSheet clears cells in the specified range of a Google Sheet.
func (w *GoogleWriter) ClearSheet(ctx context.Context, fileID, clearRange string) error {
	_, err := w.sheetsService.Spreadsheets.Values.Clear(fileID, clearRange, &sheets.ClearValuesRequest{}).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("clear sheet: %w", err)
	}

	w.logger.Info("cleared sheet range", "id", fileID, "range", clearRange)
	return nil
}

// ShareFile shares a Google Drive file with a user by email.
// role defaults to "reader" if empty. Valid values: "reader", "writer", "commenter".
func (w *GoogleWriter) ShareFile(ctx context.Context, fileID, email, role string) error {
	if role == "" {
		role = "reader"
	}

	perm := &drive.Permission{
		Type:         "user",
		Role:         role,
		EmailAddress: email,
	}

	_, err := w.service.Permissions.Create(fileID, perm).
		SupportsAllDrives(true).
		SendNotificationEmail(true).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("share drive file: %w", err)
	}

	w.logger.Info("shared drive file", "id", fileID, "email", email, "role", role)
	return nil
}

// DeleteSlide deletes a slide from a Google Slides presentation.
// If slideID is empty, deletes the last slide.
func (w *GoogleWriter) DeleteSlide(ctx context.Context, fileID, slideID string) error {
	if slideID == "" {
		pres, err := w.slidesService.Presentations.Get(fileID).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get presentation: %w", err)
		}
		if len(pres.Slides) == 0 {
			return fmt.Errorf("presentation has no slides")
		}
		slideID = pres.Slides[len(pres.Slides)-1].ObjectId
	}

	req := &slides.BatchUpdatePresentationRequest{
		Requests: []*slides.Request{
			{
				DeleteObject: &slides.DeleteObjectRequest{
					ObjectId: slideID,
				},
			},
		},
	}

	_, err := w.slidesService.Presentations.BatchUpdate(fileID, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("delete slide: %w", err)
	}

	w.logger.Info("deleted slide", "presentation_id", fileID, "slide_id", slideID)
	return nil
}

// AddSheetTab adds a new sheet tab to an existing Google Spreadsheet.
func (w *GoogleWriter) AddSheetTab(ctx context.Context, fileID, tabName string) error {
	req := &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{
						Title: tabName,
					},
				},
			},
		},
	}

	_, err := w.sheetsService.Spreadsheets.BatchUpdate(fileID, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("add sheet tab: %w", err)
	}

	w.logger.Info("added sheet tab", "id", fileID, "tab_name", tabName)
	return nil
}

// ExportPDF exports a Google Drive file as PDF and uploads it back to Drive.
// If fileName is empty, defaults to "export.pdf".
// Returns the webViewLink of the uploaded PDF.
func (w *GoogleWriter) ExportPDF(ctx context.Context, fileID, fileName string) (string, error) {
	if fileName == "" {
		fileName = "export.pdf"
	}

	resp, err := w.service.Files.Export(fileID, "application/pdf").Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("export file as pdf: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read pdf export body: %w", err)
	}

	url, err := w.UploadFile(ctx, fileName, data, "application/pdf", "")
	if err != nil {
		return "", fmt.Errorf("upload exported pdf: %w", err)
	}

	w.logger.Info("exported file as pdf", "source_id", fileID, "file_name", fileName, "url", url)
	return url, nil
}

// CreateEvent creates a new Google Calendar event.
func (w *GoogleWriter) CreateEvent(ctx context.Context, calendarID, summary, description, location, startTime, endTime string, attendees []string) (string, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	event := &calendar.Event{
		Summary:     summary,
		Description: description,
		Location:    location,
	}
	// Support both date-only (all-day) and datetime formats
	if len(startTime) <= 10 {
		event.Start = &calendar.EventDateTime{Date: startTime}
	} else {
		event.Start = &calendar.EventDateTime{DateTime: startTime}
	}
	if len(endTime) <= 10 {
		event.End = &calendar.EventDateTime{Date: endTime}
	} else {
		event.End = &calendar.EventDateTime{DateTime: endTime}
	}
	for _, email := range attendees {
		event.Attendees = append(event.Attendees, &calendar.EventAttendee{Email: email})
	}
	created, err := w.calendarService.Events.Insert(calendarID, event).Context(ctx).SendUpdates("all").Do()
	if err != nil {
		return "", fmt.Errorf("create calendar event: %w", err)
	}
	w.logger.Info("created calendar event", "summary", summary, "id", created.Id, "url", created.HtmlLink)
	return created.HtmlLink, nil
}

// EditEvent updates an existing Google Calendar event. Only non-empty fields are updated.
func (w *GoogleWriter) EditEvent(ctx context.Context, calendarID, eventID, summary, description, location, startTime, endTime string, attendees []string) (string, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	// Fetch existing event
	existing, err := w.calendarService.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get calendar event: %w", err)
	}
	if summary != "" {
		existing.Summary = summary
	}
	if description != "" {
		existing.Description = description
	}
	if location != "" {
		existing.Location = location
	}
	if startTime != "" {
		if len(startTime) <= 10 {
			existing.Start = &calendar.EventDateTime{Date: startTime}
		} else {
			existing.Start = &calendar.EventDateTime{DateTime: startTime}
		}
	}
	if endTime != "" {
		if len(endTime) <= 10 {
			existing.End = &calendar.EventDateTime{Date: endTime}
		} else {
			existing.End = &calendar.EventDateTime{DateTime: endTime}
		}
	}
	if len(attendees) > 0 {
		existing.Attendees = nil
		for _, email := range attendees {
			existing.Attendees = append(existing.Attendees, &calendar.EventAttendee{Email: email})
		}
	}
	updated, err := w.calendarService.Events.Update(calendarID, eventID, existing).Context(ctx).SendUpdates("all").Do()
	if err != nil {
		return "", fmt.Errorf("update calendar event: %w", err)
	}
	w.logger.Info("updated calendar event", "id", eventID, "url", updated.HtmlLink)
	return updated.HtmlLink, nil
}

// DeleteEvent deletes a Google Calendar event.
func (w *GoogleWriter) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	if calendarID == "" {
		calendarID = "primary"
	}
	if err := w.calendarService.Events.Delete(calendarID, eventID).Context(ctx).SendUpdates("all").Do(); err != nil {
		return fmt.Errorf("delete calendar event: %w", err)
	}
	w.logger.Info("deleted calendar event", "id", eventID)
	return nil
}

// ListEvents lists Google Calendar events within a time range.
func (w *GoogleWriter) ListEvents(ctx context.Context, calendarID, timeMin, timeMax string, maxResults int) ([]map[string]any, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	if maxResults <= 0 {
		maxResults = 20
	}
	req := w.calendarService.Events.List(calendarID).Context(ctx).
		SingleEvents(true).
		OrderBy("startTime").
		MaxResults(int64(maxResults))
	if timeMin != "" {
		req = req.TimeMin(timeMin)
	}
	if timeMax != "" {
		req = req.TimeMax(timeMax)
	}
	events, err := req.Do()
	if err != nil {
		return nil, fmt.Errorf("list calendar events: %w", err)
	}
	var results []map[string]any
	for _, ev := range events.Items {
		start := ev.Start.DateTime
		if start == "" {
			start = ev.Start.Date
		}
		end := ev.End.DateTime
		if end == "" {
			end = ev.End.Date
		}
		var attendeeEmails []string
		for _, a := range ev.Attendees {
			attendeeEmails = append(attendeeEmails, a.Email)
		}
		results = append(results, map[string]any{
			"id":          ev.Id,
			"summary":     ev.Summary,
			"description": ev.Description,
			"location":    ev.Location,
			"start":       start,
			"end":         end,
			"url":         ev.HtmlLink,
			"attendees":   attendeeEmails,
			"status":      ev.Status,
		})
	}
	w.logger.Info("listed calendar events", "calendar", calendarID, "count", len(results))
	return results, nil
}

// AddAttendees adds attendees to an existing calendar event.
func (w *GoogleWriter) AddAttendees(ctx context.Context, calendarID, eventID string, attendees []string) error {
	if calendarID == "" {
		calendarID = "primary"
	}
	existing, err := w.calendarService.Events.Get(calendarID, eventID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("get calendar event: %w", err)
	}
	for _, email := range attendees {
		existing.Attendees = append(existing.Attendees, &calendar.EventAttendee{Email: email})
	}
	if _, err := w.calendarService.Events.Update(calendarID, eventID, existing).Context(ctx).SendUpdates("all").Do(); err != nil {
		return fmt.Errorf("add attendees: %w", err)
	}
	w.logger.Info("added attendees to event", "id", eventID, "added", len(attendees))
	return nil
}

// ListCalendars lists all accessible Google Calendars.
func (w *GoogleWriter) ListCalendars(ctx context.Context) ([]map[string]any, error) {
	list, err := w.calendarService.CalendarList.List().Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	var results []map[string]any
	for _, cal := range list.Items {
		results = append(results, map[string]any{
			"id":          cal.Id,
			"summary":     cal.Summary,
			"description": cal.Description,
			"primary":     cal.Primary,
			"access_role": cal.AccessRole,
		})
	}
	w.logger.Info("listed calendars", "count", len(results))
	return results, nil
}

// CreateTask creates a new Google Task.
func (w *GoogleWriter) CreateTask(ctx context.Context, taskListID, title, notes, due string) (string, error) {
	if taskListID == "" {
		taskListID = "@default"
	}
	task := &tasks.Task{
		Title: title,
		Notes: notes,
	}
	if due != "" {
		// Ensure RFC3339 format
		if len(due) <= 10 {
			due = due + "T00:00:00Z"
		}
		task.Due = due
	}
	created, err := w.tasksService.Tasks.Insert(taskListID, task).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	w.logger.Info("created task", "title", title, "id", created.Id)
	return created.Id, nil
}

// EditTask updates an existing Google Task.
func (w *GoogleWriter) EditTask(ctx context.Context, taskListID, taskID, title, notes, due, status string) error {
	if taskListID == "" {
		taskListID = "@default"
	}
	existing, err := w.tasksService.Tasks.Get(taskListID, taskID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("get task: %w", err)
	}
	if title != "" {
		existing.Title = title
	}
	if notes != "" {
		existing.Notes = notes
	}
	if due != "" {
		if len(due) <= 10 {
			due = due + "T00:00:00Z"
		}
		existing.Due = due
	}
	if status != "" {
		existing.Status = status // "needsAction" or "completed"
	}
	if _, err := w.tasksService.Tasks.Update(taskListID, taskID, existing).Context(ctx).Do(); err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	w.logger.Info("updated task", "id", taskID)
	return nil
}

// DeleteTask deletes a Google Task.
func (w *GoogleWriter) DeleteTask(ctx context.Context, taskListID, taskID string) error {
	if taskListID == "" {
		taskListID = "@default"
	}
	if err := w.tasksService.Tasks.Delete(taskListID, taskID).Context(ctx).Do(); err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	w.logger.Info("deleted task", "id", taskID)
	return nil
}

// ListTasks lists tasks in a task list.
func (w *GoogleWriter) ListTasks(ctx context.Context, taskListID string, maxResults int) ([]map[string]any, error) {
	if taskListID == "" {
		taskListID = "@default"
	}
	if maxResults <= 0 {
		maxResults = 20
	}
	resp, err := w.tasksService.Tasks.List(taskListID).Context(ctx).MaxResults(int64(maxResults)).Do()
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var results []map[string]any
	for _, t := range resp.Items {
		results = append(results, map[string]any{
			"id":      t.Id,
			"title":   t.Title,
			"notes":   t.Notes,
			"due":     t.Due,
			"status":  t.Status,
			"updated": t.Updated,
		})
	}
	w.logger.Info("listed tasks", "list", taskListID, "count", len(results))
	return results, nil
}

// ListTaskLists lists all task lists.
func (w *GoogleWriter) ListTaskLists(ctx context.Context) ([]map[string]any, error) {
	resp, err := w.tasksService.Tasklists.List().Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("list task lists: %w", err)
	}
	var results []map[string]any
	for _, tl := range resp.Items {
		results = append(results, map[string]any{
			"id":    tl.Id,
			"title": tl.Title,
		})
	}
	w.logger.Info("listed task lists", "count", len(results))
	return results, nil
}

// SendEmail sends an email via Gmail API.
func (w *GoogleWriter) SendEmail(ctx context.Context, to, cc, subject, body string) error {
	header := make(map[string]string)
	from := w.senderEmail
	if from == "" {
		from = "me"
	}
	header["From"] = from
	header["To"] = to
	if cc != "" {
		header["Cc"] = cc
	}
	header["Subject"] = subject
	header["MIME-Version"] = "1.0"
	header["Content-Type"] = "text/plain; charset=\"utf-8\""

	var msg strings.Builder
	for k, v := range header {
		msg.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	msg.WriteString("\r\n")
	msg.WriteString(body)

	raw := base64.URLEncoding.EncodeToString([]byte(msg.String()))
	gmailMsg := &gmail.Message{Raw: raw}
	_, err := w.gmailService.Users.Messages.Send("me", gmailMsg).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	w.logger.Info("sent email", "to", to, "subject", subject)
	return nil
}
