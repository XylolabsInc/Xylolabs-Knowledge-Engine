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
				Description: "Google Drive에 새 Google Docs 문서를 생성합니다. 회의록, 보고서, 메모 등 텍스트 문서를 만들 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "문서 제목",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "문서 내용 (Markdown 형식 사용 가능: 제목, 굵게, 목록, 링크 등)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive 폴더 ID (비워두면 기본 공유 드라이브에 생성)",
						},
					},
					"required": []string{"title", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_drive_folder",
				Description: "Google Drive에 새 폴더를 생성합니다. 여러 파일을 정리해서 업로드할 때 먼저 폴더를 만들고 그 안에 파일을 넣을 수 있습니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"description": "폴더 이름",
						},
						"parent_folder_id": map[string]any{
							"type":        "string",
							"description": "상위 폴더 ID (비워두면 기본 공유 드라이브에 생성)",
						},
					},
					"required": []string{"name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "upload_to_drive",
				Description: "첨부된 파일을 Google Drive에 업로드합니다. file_name을 지정하면 해당 파일만, 비워두면 모든 첨부파일을 한번에 업로드합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_name": map[string]any{
							"type":        "string",
							"description": "업로드할 파일 이름 (비워두면 모든 첨부파일 업로드)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive 폴더 ID (비워두면 기본 공유 드라이브에 업로드)",
						},
					},
					"required": []string{},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_drive_file",
				Description: "Google Drive에서 파일이나 폴더를 삭제합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "삭제할 파일/폴더의 Google Drive ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "rename_drive_file",
				Description: "Google Drive 파일이나 폴더의 이름을 변경합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "이름을 변경할 파일/폴더의 Google Drive ID",
						},
						"new_name": map[string]any{
							"type":        "string",
							"description": "새 이름",
						},
					},
					"required": []string{"file_id", "new_name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_google_doc",
				Description: "기존 Google Docs 문서의 내용을 교체합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "수정할 Google Doc의 Drive ID",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "새 내용 (Markdown 형식 사용 가능: 제목, 굵게, 목록, 링크 등, 기존 내용을 대체)",
						},
					},
					"required": []string{"file_id", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "search_drive",
				Description: "Google Drive에서 파일을 이름으로 검색합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "검색어 (파일 이름에 포함된 텍스트)",
						},
					},
					"required": []string{"query"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "get_drive_file_info",
				Description: "Google Drive 파일의 메타데이터(이름, 타입, URL, 수정일시)를 조회합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Drive 파일 ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_doc",
				Description: "Google Docs 문서의 내용을 텍스트로 읽어옵니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Doc의 Drive ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_sheet",
				Description: "Google Sheets 스프레드시트의 데이터를 읽어옵니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "읽을 범위 (예: 'Sheet1', 'Sheet1!A1:D10'). 비워두면 'Sheet1' 전체를 읽음",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_google_sheet",
				Description: "새 Google Sheets 스프레드시트를 생성합니다. 초기 데이터를 넣을 수 있습니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "스프레드시트 제목",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "초기 데이터 (JSON 2D 배열, 예: [[\"이름\",\"나이\"],[\"홍길동\",\"30\"]]). 비워두면 빈 시트 생성",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive 폴더 ID (비워두면 기본 공유 드라이브에 생성)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_google_sheet",
				Description: "Google Sheets의 특정 범위에 데이터를 씁니다 (기존 데이터 덮어쓰기).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "쓸 범위 (예: 'Sheet1!A1:C3')",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "데이터 (JSON 2D 배열, 예: [[\"이름\",\"나이\"],[\"홍길동\",\"30\"]])",
						},
					},
					"required": []string{"file_id", "range", "data"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "append_google_sheet",
				Description: "Google Sheets에 새 행을 추가합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "추가할 위치 (예: 'Sheet1')",
						},
						"data": map[string]any{
							"type":        "string",
							"description": "추가할 행 데이터 (JSON 2D 배열, 예: [[\"홍길동\",\"30\"],[\"김철수\",\"25\"]])",
						},
					},
					"required": []string{"file_id", "data"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "read_google_slides",
				Description: "Google Slides 프레젠테이션의 내용을 텍스트로 읽어옵니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Slides의 Drive ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "create_google_slides",
				Description: "새 Google Slides 프레젠테이션을 생성합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "프레젠테이션 제목",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "Google Drive 폴더 ID (비워두면 기본 공유 드라이브에 생성)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_slide",
				Description: "기존 Google Slides 프레젠테이션에 새 슬라이드를 추가합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Slides의 Drive ID",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "슬라이드 제목",
						},
						"body": map[string]any{
							"type":        "string",
							"description": "슬라이드 본문 내용",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "move_drive_file",
				Description: "Google Drive 파일을 다른 폴더로 이동합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "이동할 파일의 Google Drive ID",
						},
						"new_folder_id": map[string]any{
							"type":        "string",
							"description": "이동할 대상 폴더의 Google Drive ID",
						},
					},
					"required": []string{"file_id", "new_folder_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "copy_drive_file",
				Description: "Google Drive 파일을 복사합니다. 템플릿 기반으로 새 문서를 만들 때 유용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "복사할 파일의 Google Drive ID",
						},
						"new_name": map[string]any{
							"type":        "string",
							"description": "복사본 이름 (비워두면 '사본: 원본이름'으로 생성)",
						},
						"folder_id": map[string]any{
							"type":        "string",
							"description": "복사본을 넣을 폴더 ID (비워두면 기본 공유 드라이브)",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_drive_folder",
				Description: "Google Drive 폴더 안의 파일 목록을 조회합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"folder_id": map[string]any{
							"type":        "string",
							"description": "조회할 폴더의 Google Drive ID",
						},
					},
					"required": []string{"folder_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "append_to_google_doc",
				Description: "기존 Google Docs 문서 끝에 내용을 추가합니다. 회의록 누적, 메모 추가 등에 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Doc의 Drive ID",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "추가할 내용 (문서 끝에 추가됨)",
						},
					},
					"required": []string{"file_id", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "get_sheet_metadata",
				Description: "Google Sheets 스프레드시트의 메타데이터를 조회합니다 (시트 탭 이름, 행/열 수 등).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "clear_google_sheet",
				Description: "Google Sheets의 특정 범위의 데이터를 삭제합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
						"range": map[string]any{
							"type":        "string",
							"description": "삭제할 범위 (예: 'Sheet1!A1:C3')",
						},
					},
					"required": []string{"file_id", "range"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "share_drive_file",
				Description: "Google Drive 파일을 특정 사용자에게 공유합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "공유할 파일의 Google Drive ID",
						},
						"email": map[string]any{
							"type":        "string",
							"description": "공유 대상 이메일 주소",
						},
						"role": map[string]any{
							"type":        "string",
							"description": "권한 수준: reader(보기), writer(편집), commenter(댓글). 비워두면 reader",
						},
					},
					"required": []string{"file_id", "email"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_slide",
				Description: "Google Slides 프레젠테이션에서 슬라이드를 삭제합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Slides의 Drive ID",
						},
						"slide_id": map[string]any{
							"type":        "string",
							"description": "삭제할 슬라이드 ID (비워두면 마지막 슬라이드 삭제)",
						},
					},
					"required": []string{"file_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_sheet_tab",
				Description: "Google Sheets 스프레드시트에 새 시트 탭을 추가합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "Google Sheets의 Drive ID",
						},
						"tab_name": map[string]any{
							"type":        "string",
							"description": "새 시트 탭 이름",
						},
					},
					"required": []string{"file_id", "tab_name"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "export_as_pdf",
				Description: "Google Drive 파일(Docs, Sheets, Slides)을 PDF로 내보내서 드라이브에 저장합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_id": map[string]any{
							"type":        "string",
							"description": "PDF로 내보낼 파일의 Google Drive ID",
						},
						"file_name": map[string]any{
							"type":        "string",
							"description": "PDF 파일명 (비워두면 'export.pdf')",
						},
					},
					"required": []string{"file_id"},
				},
			},
			// --- Calendar Tools ---
			gemini.FunctionDeclaration{
				Name:        "create_calendar_event",
				Description: "Google Calendar에 새 이벤트를 생성합니다. 미팅, 회의, 약속 등을 잡을 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary": map[string]any{
							"type":        "string",
							"description": "이벤트 제목",
						},
						"start_time": map[string]any{
							"type":        "string",
							"description": "시작 시간 (RFC3339 형식: 2024-01-15T09:00:00+09:00 또는 종일 이벤트: 2024-01-15)",
						},
						"end_time": map[string]any{
							"type":        "string",
							"description": "종료 시간 (RFC3339 형식: 2024-01-15T10:00:00+09:00 또는 종일 이벤트: 2024-01-16)",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "캘린더 ID (비워두면 기본 캘린더 사용)",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "이벤트 설명/메모",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "장소",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "참석자 이메일 주소 (쉼표로 구분, 예: a@co.com,b@co.com)",
						},
					},
					"required": []string{"summary", "start_time", "end_time"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_calendar_event",
				Description: "기존 Google Calendar 이벤트를 수정합니다. 제목, 시간, 장소, 참석자 등을 변경할 수 있습니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "수정할 이벤트 ID",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "캘린더 ID (비워두면 기본 캘린더)",
						},
						"summary": map[string]any{
							"type":        "string",
							"description": "새 이벤트 제목",
						},
						"description": map[string]any{
							"type":        "string",
							"description": "새 이벤트 설명",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "새 장소",
						},
						"start_time": map[string]any{
							"type":        "string",
							"description": "새 시작 시간 (RFC3339)",
						},
						"end_time": map[string]any{
							"type":        "string",
							"description": "새 종료 시간 (RFC3339)",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "새 참석자 목록 (쉼표 구분, 기존 목록을 대체)",
						},
					},
					"required": []string{"event_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_calendar_event",
				Description: "Google Calendar 이벤트를 삭제합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "삭제할 이벤트 ID",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "캘린더 ID (비워두면 기본 캘린더)",
						},
					},
					"required": []string{"event_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_calendar_events",
				Description: "Google Calendar 이벤트 목록을 조회합니다. 특정 기간의 일정을 확인할 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "캘린더 ID (비워두면 기본 캘린더)",
						},
						"time_min": map[string]any{
							"type":        "string",
							"description": "조회 시작 시간 (RFC3339)",
						},
						"time_max": map[string]any{
							"type":        "string",
							"description": "조회 종료 시간 (RFC3339)",
						},
						"max_results": map[string]any{
							"type":        "number",
							"description": "최대 결과 수 (기본값: 20)",
						},
					},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "add_event_attendees",
				Description: "기존 캘린더 이벤트에 참석자를 추가합니다. 초대 메일이 자동 발송됩니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"event_id": map[string]any{
							"type":        "string",
							"description": "이벤트 ID",
						},
						"attendees": map[string]any{
							"type":        "string",
							"description": "추가할 참석자 이메일 (쉼표 구분)",
						},
						"calendar_id": map[string]any{
							"type":        "string",
							"description": "캘린더 ID (비워두면 기본 캘린더)",
						},
					},
					"required": []string{"event_id", "attendees"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_calendars",
				Description: "접근 가능한 Google Calendar 목록을 조회합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{},
				},
			},
			// --- Tasks Tools ---
			gemini.FunctionDeclaration{
				Name:        "create_task",
				Description: "Google Tasks에 새 할 일을 추가합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "할 일 제목",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "태스크 리스트 ID (비워두면 기본 리스트)",
						},
						"notes": map[string]any{
							"type":        "string",
							"description": "메모/상세 내용",
						},
						"due": map[string]any{
							"type":        "string",
							"description": "마감일 (RFC3339 또는 YYYY-MM-DD)",
						},
					},
					"required": []string{"title"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "edit_task",
				Description: "기존 Google Tasks 할 일을 수정합니다. 제목, 메모, 마감일, 완료 상태를 변경할 수 있습니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{
							"type":        "string",
							"description": "수정할 태스크 ID",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "태스크 리스트 ID (비워두면 기본 리스트)",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "새 제목",
						},
						"notes": map[string]any{
							"type":        "string",
							"description": "새 메모",
						},
						"due": map[string]any{
							"type":        "string",
							"description": "새 마감일",
						},
						"status": map[string]any{
							"type":        "string",
							"description": "상태 변경 (needsAction: 미완료, completed: 완료)",
						},
					},
					"required": []string{"task_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "delete_task",
				Description: "Google Tasks 할 일을 삭제합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{
							"type":        "string",
							"description": "삭제할 태스크 ID",
						},
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "태스크 리스트 ID (비워두면 기본 리스트)",
						},
					},
					"required": []string{"task_id"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_tasks",
				Description: "Google Tasks 할 일 목록을 조회합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_list_id": map[string]any{
							"type":        "string",
							"description": "태스크 리스트 ID (비워두면 기본 리스트)",
						},
						"max_results": map[string]any{
							"type":        "number",
							"description": "최대 결과 수 (기본값: 20)",
						},
					},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "list_task_lists",
				Description: "Google Tasks 태스크 리스트 목록을 조회합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{},
				},
			},
			// --- Gmail Tool ---
			gemini.FunctionDeclaration{
				Name:        "send_email",
				Description: "이메일을 발송합니다. 업무 관련 이메일, 초대, 알림 등을 보낼 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"to": map[string]any{
							"type":        "string",
							"description": "수신자 이메일 주소 (쉼표로 여러 명 가능)",
						},
						"subject": map[string]any{
							"type":        "string",
							"description": "이메일 제목",
						},
						"body": map[string]any{
							"type":        "string",
							"description": "이메일 본문",
						},
						"cc": map[string]any{
							"type":        "string",
							"description": "참조(CC) 이메일 주소 (쉼표로 여러 명 가능)",
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
				Description: "Notion에 새 페이지를 생성합니다. 회의록, 문서, 메모 등을 Notion에 작성할 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"description": "페이지 제목",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "페이지 내용 (plain text, 마크다운 헤딩/리스트 지원)",
						},
						"parent_page_id": map[string]any{
							"type":        "string",
							"description": "부모 페이지 ID (비워두면 기본 루트 페이지 하위에 생성)",
						},
					},
					"required": []string{"title", "content"},
				},
			},
			gemini.FunctionDeclaration{
				Name:        "update_notion_page",
				Description: "기존 Notion 페이지에 내용을 추가합니다. 이미 존재하는 페이지에 새로운 내용을 덧붙일 때 사용합니다.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page_id": map[string]any{
							"type":        "string",
							"description": "Notion 페이지 ID",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "추가할 내용 (plain text, 마크다운 헤딩/리스트 지원)",
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
			return nil, fmt.Errorf("첨부된 파일이 없습니다")
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
