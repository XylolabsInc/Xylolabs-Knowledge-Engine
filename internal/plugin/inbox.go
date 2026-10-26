package plugin

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxInboxNameRunes = 100

// inboxDir is where the attachments for one call or action live.
func inboxDir(p Plugin, id string) string {
	if p.DataDir() == "" || id == "" {
		return ""
	}
	return filepath.Join(p.DataDir(), "inbox", id)
}

// writeInbox saves files under <DataDir>/inbox/<id>/ and describes them for
// the plugin. It writes nothing when there are no files or the plugin has no
// data directory.
func writeInbox(p Plugin, id string, files map[string][]byte) ([]Attachment, error) {
	dir := inboxDir(p, id)
	if len(files) == 0 || dir == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("plugin: inbox: resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("plugin: inbox: create %s: %w", abs, err)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	used := map[string]bool{}
	atts := make([]Attachment, 0, len(names))
	for _, name := range names {
		data := files[name]
		safe := uniqueName(sanitizeInboxName(name), used)
		full := filepath.Join(abs, safe)
		if err := os.WriteFile(full, data, 0o600); err != nil {
			_ = os.RemoveAll(abs)
			return nil, fmt.Errorf("plugin: inbox: write %s: %w", safe, err)
		}
		atts = append(atts, Attachment{Name: safe, MimeType: sniffMIME(safe, data), Path: full})
	}
	return atts, nil
}

// readInbox loads saved attachments back into memory, keyed by name.
func readInbox(atts []Attachment) (map[string][]byte, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	files := make(map[string][]byte, len(atts))
	for _, a := range atts {
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, fmt.Errorf("plugin: inbox: read %s: %w", a.Name, err)
		}
		files[a.Name] = data
	}
	return files, nil
}

// sanitizeInboxName reduces a user-supplied file name to a bare, safe name.
func sanitizeInboxName(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*/`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "file"
	}
	if utf8.RuneCountInString(name) > maxInboxNameRunes {
		ext := path.Ext(name)
		if utf8.RuneCountInString(ext) > 10 {
			ext = ""
		}
		stem := []rune(strings.TrimSuffix(name, ext))
		name = string(stem[:maxInboxNameRunes-utf8.RuneCountInString(ext)]) + ext
	}
	return name
}

// uniqueName appends " (2)", " (3)", … before the extension until name is unused.
func uniqueName(name string, used map[string]bool) string {
	candidate := name
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; used[strings.ToLower(candidate)]; i++ {
		candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
	used[strings.ToLower(candidate)] = true
	return candidate
}

// extraTypes covers formats phones produce that the mime package may not know.
var extraTypes = map[string]string{".heic": "image/heic", ".heif": "image/heif"}

// sniffMIME reports a file's media type from its bytes, falling back to the
// extension when the bytes say only "binary" or "text".
func sniffMIME(name string, data []byte) string {
	sniffed, _, _ := strings.Cut(http.DetectContentType(data), ";")
	if sniffed != "application/octet-stream" && sniffed != "text/plain" {
		return sniffed
	}
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return sniffed
}
