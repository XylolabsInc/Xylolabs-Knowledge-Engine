package tools

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxOutgoingFiles     = 5
	maxOutgoingFileBytes = 1 << 20 // 1 MB per file
	maxOutgoingNameRunes = 100
)

// OutgoingFile is a file a tool produced for the bot to upload alongside its reply.
type OutgoingFile struct {
	Name string
	Data []byte
}

// Outbox collects the files tools produce while answering one message. It
// lives in the request context rather than on ToolExecutor, which is shared by
// every concurrent request, so one user's file never lands in another's thread.
type Outbox struct {
	mu    sync.Mutex
	files []OutgoingFile
}

type outboxCtxKey struct{}

// ContextWithOutbox returns a copy of ctx carrying a fresh Outbox, and the Outbox.
func ContextWithOutbox(ctx context.Context) (context.Context, *Outbox) {
	ob := &Outbox{}
	return context.WithValue(ctx, outboxCtxKey{}, ob), ob
}

func outboxFromContext(ctx context.Context) *Outbox {
	ob, _ := ctx.Value(outboxCtxKey{}).(*Outbox)
	return ob
}

// Files returns the files added so far.
func (o *Outbox) Files() []OutgoingFile {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]OutgoingFile(nil), o.files...)
}

// add stores a file, replacing an earlier one with the same name so a model
// that regenerates a file after a correction sends only the final version.
func (o *Outbox) add(f OutgoingFile) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.files {
		if o.files[i].Name == f.Name {
			o.files[i] = f
			return nil
		}
	}
	if len(o.files) >= maxOutgoingFiles {
		return fmt.Errorf("at most %d files can be attached to one reply", maxOutgoingFiles)
	}
	o.files = append(o.files, f)
	return nil
}

// sanitizeFileName reduces a model-supplied name to a bare file name with an
// extension, so it cannot carry a path and always tells the platform its type.
func sanitizeFileName(name string) (string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || name == "/" {
		return "", fmt.Errorf("file_name is required")
	}
	ext := path.Ext(name)
	if ext == "" || ext == name {
		return "", fmt.Errorf("file_name %q needs an extension such as .ics, .csv or .txt", name)
	}
	if utf8.RuneCountInString(name) > maxOutgoingNameRunes {
		stem := []rune(strings.TrimSuffix(name, ext))
		keep := maxOutgoingNameRunes - utf8.RuneCountInString(ext)
		if keep < 1 {
			return "", fmt.Errorf("file_name extension is too long")
		}
		name = string(stem[:min(keep, len(stem))]) + ext
	}
	return name, nil
}

// crlfExtensions are formats whose specs require CRLF line endings
// (RFC 5545 for iCalendar, RFC 6350 for vCard). Models write bare LF, and
// some calendar apps reject or misparse an .ics file that uses it.
var crlfExtensions = map[string]bool{".ics": true, ".ical": true, ".ifb": true, ".vcs": true, ".vcf": true}

// normalizeFileContent applies the line-ending rules the file's format demands.
func normalizeFileContent(name, content string) string {
	if !crlfExtensions[strings.ToLower(path.Ext(name))] {
		return content
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimRight(content, "\n")
	return strings.ReplaceAll(content, "\n", "\r\n") + "\r\n"
}

// attachFile validates a model-generated text file and queues it on the
// request's Outbox for upload with the reply.
func attachFile(ctx context.Context, args map[string]any) (map[string]any, error) {
	ob := outboxFromContext(ctx)
	if ob == nil {
		return nil, fmt.Errorf("files cannot be attached here")
	}
	rawName, _ := args["file_name"].(string)
	content, _ := args["content"].(string)
	name, err := sanitizeFileName(rawName)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("content is required")
	}
	if !utf8.ValidString(content) {
		return nil, fmt.Errorf("content must be UTF-8 text")
	}
	data := []byte(normalizeFileContent(name, content))
	if len(data) > maxOutgoingFileBytes {
		return nil, fmt.Errorf("file is %d bytes, over the %d byte limit", len(data), maxOutgoingFileBytes)
	}
	if err := ob.add(OutgoingFile{Name: name, Data: data}); err != nil {
		return nil, err
	}
	return map[string]any{
		"status":     "attached",
		"file_name":  name,
		"size_bytes": len(data),
		"message":    "The file will be uploaded to the conversation together with your reply. Mention it briefly; do not paste its contents again.",
	}, nil
}
