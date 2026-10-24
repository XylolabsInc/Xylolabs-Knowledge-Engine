package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/xylolabsinc/xylolabs-kb/internal/gemini"
)

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain name kept", in: "meeting.ics", want: "meeting.ics"},
		{name: "korean name kept", in: "주간 회의.ics", want: "주간 회의.ics"},
		{name: "unix path stripped", in: "../../etc/passwd.txt", want: "passwd.txt"},
		{name: "windows path stripped", in: `C:\Users\x\notes.md`, want: "notes.md"},
		{name: "reserved characters replaced", in: `a:b?.csv`, want: "a_b_.csv"},
		{name: "surrounding space trimmed", in: "  data.json  ", want: "data.json"},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "dots only rejected", in: "..", wantErr: true},
		{name: "missing extension rejected", in: "calendar", wantErr: true},
		{name: "dotfile without stem rejected", in: ".ics", wantErr: true},
		{name: "long name shortened keeping extension", in: strings.Repeat("가", 150) + ".ics", want: strings.Repeat("가", 96) + ".ics"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeFileName(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sanitizeFileName(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeFileName(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("sanitizeFileName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeFileContent(t *testing.T) {
	tests := []struct {
		name string
		file string
		in   string
		want string
	}{
		{name: "ics LF becomes CRLF", file: "a.ics", in: "BEGIN:VCALENDAR\nEND:VCALENDAR", want: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"},
		{name: "ics existing CRLF not doubled", file: "a.ics", in: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", want: "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"},
		{name: "uppercase vcf extension handled", file: "card.VCF", in: "BEGIN:VCARD\nEND:VCARD\n\n", want: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
		{name: "csv left untouched", file: "a.csv", in: "a,b\n1,2\n", want: "a,b\n1,2\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeFileContent(tt.file, tt.in); got != tt.want {
				t.Errorf("normalizeFileContent(%q) = %q, want %q", tt.file, got, tt.want)
			}
		})
	}
}

func TestAttachFile(t *testing.T) {
	t.Run("without an outbox the call fails", func(t *testing.T) {
		_, err := attachFile(context.Background(), map[string]any{"file_name": "a.txt", "content": "x"})
		if err == nil {
			t.Fatal("expected error without outbox")
		}
	})

	t.Run("file is queued with normalized content", func(t *testing.T) {
		ctx, ob := ContextWithOutbox(context.Background())
		res, err := attachFile(ctx, map[string]any{"file_name": "meeting.ics", "content": "BEGIN:VCALENDAR\nEND:VCALENDAR"})
		if err != nil {
			t.Fatal(err)
		}
		if res["file_name"] != "meeting.ics" {
			t.Errorf("file_name = %v", res["file_name"])
		}
		files := ob.Files()
		if len(files) != 1 || string(files[0].Data) != "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" {
			t.Fatalf("outbox = %+v", files)
		}
	})

	t.Run("same name replaces the earlier file", func(t *testing.T) {
		ctx, ob := ContextWithOutbox(context.Background())
		for _, c := range []string{"first", "second"} {
			if _, err := attachFile(ctx, map[string]any{"file_name": "a.txt", "content": c}); err != nil {
				t.Fatal(err)
			}
		}
		files := ob.Files()
		if len(files) != 1 || string(files[0].Data) != "second" {
			t.Fatalf("outbox = %+v", files)
		}
	})

	t.Run("file count is capped", func(t *testing.T) {
		ctx, _ := ContextWithOutbox(context.Background())
		var err error
		for i := 0; i <= maxOutgoingFiles; i++ {
			_, err = attachFile(ctx, map[string]any{"file_name": string(rune('a'+i)) + ".txt", "content": "x"})
		}
		if err == nil {
			t.Fatal("expected error past the file cap")
		}
	})

	t.Run("empty and oversized content rejected", func(t *testing.T) {
		ctx, ob := ContextWithOutbox(context.Background())
		for _, c := range []string{"", "  \n", strings.Repeat("x", maxOutgoingFileBytes+1)} {
			if _, err := attachFile(ctx, map[string]any{"file_name": "a.txt", "content": c}); err == nil {
				t.Errorf("content of length %d accepted", len(c))
			}
		}
		if len(ob.Files()) != 0 {
			t.Errorf("rejected files reached the outbox: %d", len(ob.Files()))
		}
	})

	t.Run("outboxes of separate requests stay apart", func(t *testing.T) {
		ctxA, obA := ContextWithOutbox(context.Background())
		_, obB := ContextWithOutbox(context.Background())
		if _, err := attachFile(ctxA, map[string]any{"file_name": "a.txt", "content": "x"}); err != nil {
			t.Fatal(err)
		}
		if len(obA.Files()) != 1 || len(obB.Files()) != 0 {
			t.Fatalf("A=%d B=%d", len(obA.Files()), len(obB.Files()))
		}
	})
}

func TestExecuteAttachFile(t *testing.T) {
	e := newTestExecutor()

	found := false
	for _, d := range e.Declarations() {
		if d.Name == "attach_file" {
			found = true
		}
	}
	if !found {
		t.Fatal("attach_file should be declared even with no services configured")
	}

	ctx, ob := ContextWithOutbox(context.Background())
	resp := e.Execute(ctx, gemini.FunctionCall{Name: "attach_file", Args: map[string]any{"file_name": "x.csv", "content": "a,b\n"}})
	if errMsg, ok := resp.Response.(map[string]any)["error"]; ok {
		t.Fatalf("Execute returned error: %v", errMsg)
	}
	if len(ob.Files()) != 1 {
		t.Fatalf("expected 1 file, got %d", len(ob.Files()))
	}
}
