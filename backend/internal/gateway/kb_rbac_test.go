package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// kbRoleFlow wires the gateway to the in-memory identity services plus
// recording ingestion/query clients so kb-role enforcement can be exercised
// end to end.
func TestGatewayKnowledgeBaseRoleMatrix(t *testing.T) {
	g := newIdentityGateway(t)
	ingestion := &recordingIngestionClient{fileSize: 10}
	query := &recordingQueryClient{}
	g.ingestion = ingestion
	g.query = query
	h := g.Router()

	admin := loginToken(t, h, "admin", "admin123")

	// admin creates a namespace and becomes its kb:admin.
	w := call(t, h, http.MethodPost, "/v1/knowledge-bases", admin, map[string]string{"name": "RBAC 网关库"})
	if w.Code != http.StatusOK {
		t.Fatalf("create kb: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		KB struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"knowledge_base"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.KB.ID == "" {
		t.Fatalf("decode kb: %v %s", err, w.Body.String())
	}
	kbID := created.KB.ID
	if created.KB.Role != "kb:admin" {
		t.Fatalf("creator role = %q, want kb:admin", created.KB.Role)
	}

	// An editor user has no binding yet: the namespace is invisible.
	w = call(t, h, http.MethodPost, "/v1/users", admin, map[string]any{
		"username": "kbeditor", "password": "pw123456", "roles": []string{"editor"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create editor: %d %s", w.Code, w.Body.String())
	}
	var userBody struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &userBody)
	editor := loginToken(t, h, "kbeditor", "pw123456")

	assertSearch := func(expected int) {
		t.Helper()
		w := call(t, h, http.MethodPost, "/v1/search", editor, map[string]any{"query": "x", "kb_id": kbID})
		if w.Code != expected {
			t.Fatalf("search = %d, want %d (%s)", w.Code, expected, w.Body.String())
		}
	}
	assertUpload := func(expected int) {
		t.Helper()
		w := call(t, h, http.MethodPost, "/v1/files", editor, map[string]any{
			"file_name": "a.txt", "file_type": "txt", "file_size": 10, "kb_id": kbID,
		})
		if w.Code != expected {
			t.Fatalf("upload = %d, want %d (%s)", w.Code, expected, w.Body.String())
		}
	}
	grant := func(role string) {
		t.Helper()
		w := call(t, h, http.MethodPost, "/v1/knowledge-bases/"+kbID+"/permissions", admin, map[string]any{
			"subject_type": "user", "subject_id": userBody.User.ID, "permission": role,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("grant %s: %d %s", role, w.Code, w.Body.String())
		}
	}

	assertSearch(http.StatusNotFound)
	assertUpload(http.StatusNotFound)

	// kb:viewer: read/search allowed, writes rejected.
	grant("kb:viewer")
	assertSearch(http.StatusOK)
	assertUpload(http.StatusForbidden)
	if w := call(t, h, http.MethodDelete, "/v1/files/task-1?kb_id="+kbID, editor, nil); w.Code != http.StatusForbidden {
		t.Errorf("viewer delete = %d, want 403", w.Code)
	}
	if w := call(t, h, http.MethodPost, "/v1/files/task-1/reindex?kb_id="+kbID, editor, nil); w.Code != http.StatusForbidden {
		t.Errorf("viewer reindex = %d, want 403", w.Code)
	}

	// kb:editor: upload, delete and reindex allowed; management still denied.
	grant("kb:editor")
	assertUpload(http.StatusAccepted)
	w = call(t, h, http.MethodDelete, "/v1/files/task-1?kb_id="+kbID, editor, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("editor delete = %d %s", w.Code, w.Body.String())
	}
	if w := call(t, h, http.MethodPost, "/v1/files/task-1/reindex?kb_id="+kbID, editor, nil); w.Code != http.StatusAccepted {
		t.Fatalf("editor reindex = %d %s", w.Code, w.Body.String())
	}
	if ingestion.reindexed.GetTaskId() != "task-1" || ingestion.reindexed.GetKbId() != kbID {
		t.Fatalf("reindex request = %+v", ingestion.reindexed)
	}
	if w := call(t, h, http.MethodPatch, "/v1/knowledge-bases/"+kbID, editor, map[string]string{"name": "x"}); w.Code != http.StatusForbidden {
		t.Errorf("editor rename = %d, want 403", w.Code)
	}
	if w := call(t, h, http.MethodPost, "/v1/knowledge-bases/"+kbID+"/permissions", editor, map[string]any{
		"subject_type": "user", "subject_id": userBody.User.ID, "permission": "kb:admin",
	}); w.Code != http.StatusForbidden {
		t.Errorf("editor grant = %d, want 403", w.Code)
	}

	// Only kb:admin can list bindings and delete the namespace. Granting the
	// editor kb:admin also lifts namespace management (no tenant kb:write needed).
	if w := call(t, h, http.MethodGet, "/v1/knowledge-bases/"+kbID+"/permissions", editor, nil); w.Code != http.StatusForbidden {
		t.Errorf("editor list permissions = %d, want 403", w.Code)
	}
	grant("kb:admin")
	if w := call(t, h, http.MethodPatch, "/v1/knowledge-bases/"+kbID, editor, map[string]string{"name": "RBAC 库改名"}); w.Code != http.StatusOK {
		t.Errorf("kb admin rename = %d %s", w.Code, w.Body.String())
	}
	w = call(t, h, http.MethodGet, "/v1/knowledge-bases/"+kbID+"/permissions", editor, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("kb admin list permissions = %d %s", w.Code, w.Body.String())
	}
	var perms struct {
		Permissions []struct {
			SubjectID  string `json:"subject_id"`
			Permission string `json:"permission"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &perms)
	if len(perms.Permissions) != 2 {
		t.Fatalf("permissions = %+v, want creator + editor binding", perms.Permissions)
	}
	if w := call(t, h, http.MethodDelete, "/v1/knowledge-bases/"+kbID, admin, nil); w.Code != http.StatusOK {
		t.Errorf("admin delete kb = %d %s", w.Code, w.Body.String())
	}
}
