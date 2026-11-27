package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/strausmann/fileee-mcp-server/internal/tools"
)

func TestReviewWritesThroughAuthenticatedMCP(t *testing.T) {
	const initial = "2026-01-02T03:04:05.456Z"
	const edited = "2026-01-02T03:04:05.457Z"
	const archived = "2026-01-02T03:04:05.458Z"
	var mu sync.Mutex
	saved := strings.Replace(reviewHTTPDocument, `"data":{`, `"data":{"reminderIds":{"type":"LIST","value":["reminder-example"]},`, 1)
	puts := 0
	mux := reviewHTTPBackend(t)
	mux.HandleFunc("GET /api/documents/rest/doc-example", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, saved)
	})
	mux.HandleFunc("PUT /api/documents/rest/doc-example", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var current map[string]json.RawMessage
		if err := json.Unmarshal(raw, &current); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if puts == 0 {
			current["modified"], _ = json.Marshal(edited)
		} else {
			current["modified"], _ = json.Marshal(archived)
		}
		updated, err := json.Marshal(current)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		saved = string(updated)
		puts++
		writeJSON(w, saved)
	})
	session := reviewHTTPClient(t, mux)
	args := map[string]any{"id": "doc-example", "fields": map[string]any{"issueDate": "2026-01-03"}, "expectedModified": initial}
	update := mustCall(t, session, tools.ToolPatchDocumentMetadata, args)
	if update.IsError || !strings.Contains(everythingTheModelCanSee(t, update), edited) {
		t.Fatal("missing verified update")
	}
	archive := map[string]any{"id": "doc-example", "archived": true, "expectedModified": initial}
	stale, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tools.ToolSetDocumentArchived, Arguments: archive})
	if err != nil || !stale.IsError {
		t.Fatalf("stale archive accepted, transport error: %v", err)
	}
	archive["expectedModified"] = edited
	finished := mustCall(t, session, tools.ToolSetDocumentArchived, archive)
	if finished.IsError || finished.StructuredContent.(map[string]any)["archived"] != true {
		t.Fatal("archive not verified")
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 2 || !strings.Contains(saved, `"value":"2026-01-03"`) || !strings.Contains(saved, `"reminder-example"`) {
		t.Fatal("unexpected save count, date, or reminder removal")
	}
}
