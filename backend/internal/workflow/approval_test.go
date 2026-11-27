package workflow

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func approvalDAG() DAG {
	return DAG{
		Name: "hitl",
		Nodes: []NodeSpec{
			{ID: "a", Type: "noop"},
			{ID: "gate", Type: "human_approval"},
			{ID: "b", Type: "noop"},
		},
		Edges: []Edge{{From: "a", To: "gate"}, {From: "gate", To: "b"}},
	}
}

func newApprovalHandler() (http.Handler, *MemoryRunStore) {
	registry := NewRegistry()
	registry.Register(NoopNode{})
	registry.Register(HumanApprovalNode{})
	store := NewMemoryRunStore(10)
	engine := NewEngine(registry, testLogger())
	return NewHTTPHandler(engine, registry, WithRunStore(store)), store
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHumanApprovalSuspendsAndResumes(t *testing.T) {
	handler, store := newApprovalHandler()

	rec := doJSON(t, handler, http.MethodPost, "/v1/workflows/run", HTTPRunRequest{
		DAG: approvalDAG(),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var result RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != string(RunWaitingApproval) {
		t.Fatalf("status = %s", result.Status)
	}
	state, ok := store.Get(result.RunID)
	if !ok || state.PendingNode != "gate" {
		t.Fatalf("checkpoint missing: %+v", state)
	}

	rec = doJSON(t, handler, http.MethodPost, "/v1/workflows/runs/"+result.RunID+"/approve",
		approvalRequest{Decision: "approve", Reason: "ok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resumed RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode resumed: %v", err)
	}
	if resumed.Status != string(RunSucceeded) {
		t.Fatalf("resumed status = %s", resumed.Status)
	}
	if len(resumed.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(resumed.Results))
	}

	rec = doJSON(t, handler, http.MethodPost, "/v1/workflows/runs/"+result.RunID+"/approve",
		approvalRequest{Decision: "approve"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409", rec.Code)
	}
}

func TestHumanApprovalRejectsRun(t *testing.T) {
	handler, _ := newApprovalHandler()

	rec := doJSON(t, handler, http.MethodPost, "/v1/workflows/run", HTTPRunRequest{
		DAG: approvalDAG(),
	})
	var result RunResult
	_ = json.Unmarshal(rec.Body.Bytes(), &result)

	rec = doJSON(t, handler, http.MethodPost, "/v1/workflows/runs/"+result.RunID+"/approve",
		approvalRequest{Decision: "reject", Reason: "not allowed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject status = %d", rec.Code)
	}
	var state RunState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state.Status != string(RunRejected) || state.Error != "not allowed" {
		t.Fatalf("state = %+v", state)
	}

	rec = doJSON(t, handler, http.MethodGet, "/v1/workflows/runs/"+result.RunID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
}

// TestConcurrentApprovalsOnlyOneWins guards the multi-replica guarantee: the
// atomic transition lets exactly one decision resume the run.
func TestConcurrentApprovalsOnlyOneWins(t *testing.T) {
	handler, _ := newApprovalHandler()
	rec := doJSON(t, handler, http.MethodPost, "/v1/workflows/run", HTTPRunRequest{
		DAG: approvalDAG(),
	})
	var result RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	path := "/v1/workflows/runs/" + result.RunID + "/approve"

	start := make(chan struct{})
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			response := doJSON(t, handler, http.MethodPost, path, approvalRequest{Decision: "approve"})
			codes <- response.Code
		}()
	}
	close(start)

	ok, conflict := 0, 0
	for i := 0; i < 2; i++ {
		switch code := <-codes; code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d, want 1/1", ok, conflict)
	}
}
