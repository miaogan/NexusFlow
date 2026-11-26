package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateReadOnlySQLAcceptsSelects(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"select id, name from users where active = true;",
		"WITH recent AS (SELECT * FROM tasks) SELECT * FROM recent",
		"-- count users\nSELECT count(*) FROM users",
		"SELECT updated_at, created_by FROM tasks",
	}
	for _, sql := range cases {
		if _, err := ValidateReadOnlySQL(sql); err != nil {
			t.Errorf("ValidateReadOnlySQL(%q) = %v, want nil", sql, err)
		}
	}
}

func TestValidateReadOnlySQLRejectsWrites(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"UPDATE users SET name = 'x'",
		"DELETE FROM users",
		"DROP TABLE users",
		"SELECT 1; DROP TABLE users",
		"SELECT * INTO backup FROM users",
		"/* wipe */ insert into users values (1)",
		"EXPLAIN SELECT 1",
		"SELECT * FROM users FOR UPDATE",
		"COPY users TO '/tmp/users.csv'",
		"WITH gone AS (DELETE FROM users RETURNING *) SELECT * FROM gone",
	}
	for _, sql := range cases {
		if _, err := ValidateReadOnlySQL(sql); !errors.Is(err, ErrSQLNotReadOnly) {
			t.Errorf("ValidateReadOnlySQL(%q) = %v, want ErrSQLNotReadOnly", sql, err)
		}
	}
}

func TestValidateReadOnlySQLStripsTrailingSemicolonAndComments(t *testing.T) {
	got, err := ValidateReadOnlySQL("  -- note\nSELECT id FROM users ;  ")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got != "SELECT id FROM users" {
		t.Errorf("guarded sql = %q", got)
	}
}

func TestNL2SQLNodeRequiresSQLOrQuestion(t *testing.T) {
	node := NewNL2SQLNode()
	_, err := node.Run(context.Background(), NewExecutionContext(nil), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "config.sql") {
		t.Fatalf("err = %v, want missing sql/question error", err)
	}

	_, err = node.Run(context.Background(), NewExecutionContext(nil), map[string]any{
		"question": "有多少用户？",
	})
	if err == nil || !strings.Contains(err.Error(), "NL2SQL_LLM_") {
		t.Fatalf("err = %v, want generator-required error", err)
	}
}

func TestNL2SQLNodeGuardsBeforeDatabaseUse(t *testing.T) {
	node := NewNL2SQLNode()
	_, err := node.Run(context.Background(), NewExecutionContext(nil), map[string]any{
		"sql": "DELETE FROM users",
	})
	if !errors.Is(err, ErrSQLNotReadOnly) {
		t.Fatalf("err = %v, want ErrSQLNotReadOnly", err)
	}

	// A valid statement reaches the database check instead.
	_, err = node.Run(context.Background(), NewExecutionContext(nil), map[string]any{
		"sql": "SELECT 1",
	})
	if err == nil || !strings.Contains(err.Error(), "database is not configured") {
		t.Fatalf("err = %v, want database configuration error", err)
	}
}

func TestNL2SQLNodeResolvesSQLFromContext(t *testing.T) {
	node := NewNL2SQLNode()
	ec := NewExecutionContext(map[string]any{"query.sql": "SELECT id FROM users"})
	_, err := node.Run(context.Background(), ec, map[string]any{"sql_from": "query.sql"})
	if err == nil || !strings.Contains(err.Error(), "database is not configured") {
		t.Fatalf("err = %v, want database configuration error", err)
	}
}

type fakeSQLGenerator struct {
	sql       string
	questions []string
}

func (g *fakeSQLGenerator) Generate(_ context.Context, question string) (string, error) {
	g.questions = append(g.questions, question)
	return g.sql, nil
}

func TestNL2SQLNodeGeneratesSQLFromQuestion(t *testing.T) {
	generator := &fakeSQLGenerator{sql: "SELECT count(*) FROM users"}
	node := NewNL2SQLNode(WithNL2SQLGenerator(generator))
	ec := NewExecutionContext(map[string]any{"question": "共有多少用户？"})

	_, err := node.Run(context.Background(), ec, map[string]any{"question_from": "question"})
	if err == nil || !strings.Contains(err.Error(), "database is not configured") {
		t.Fatalf("err = %v, want database configuration error", err)
	}
	if len(generator.questions) != 1 || generator.questions[0] != "共有多少用户？" {
		t.Fatalf("generator questions = %v", generator.questions)
	}
}

func TestHTTPNL2SQLGeneratorCallsLLM(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		body := "{\"choices\":[{\"message\":{\"content\":\"```sql\\nSELECT id FROM users LIMIT 3\\n```\"}}]}"
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	generator := NewHTTPNL2SQLGenerator(server.URL, "sk-test", "test-model", "users(id, name)", time.Second)
	sql, err := generator.Generate(context.Background(), "列出前三个用户")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if sql != "SELECT id FROM users LIMIT 3" {
		t.Errorf("sql = %q, want code fence stripped", sql)
	}
	if got["model"] != "test-model" {
		t.Errorf("model = %v", got["model"])
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	system, _ := messages[0].(map[string]any)
	if !strings.Contains(system["content"].(string), "users(id, name)") {
		t.Errorf("schema hint missing from system prompt: %v", system["content"])
	}
}

func TestHTTPNL2SQLGeneratorSurfacesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	generator := NewHTTPNL2SQLGenerator(server.URL, "sk-test", "m", "", time.Second)
	if _, err := generator.Generate(context.Background(), "q"); err == nil {
		t.Fatal("expected an error for HTTP 502")
	}

	empty := NewHTTPNL2SQLGenerator("", "", "m", "", time.Second)
	if _, err := empty.Generate(context.Background(), "q"); err == nil {
		t.Fatal("expected an error when the LLM is not configured")
	}
}

func TestNL2SQLNodeIsRegisteredForDAGs(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewNL2SQLNode())
	engine := NewEngine(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dag := &DAG{Nodes: []NodeSpec{{ID: "ask", Type: "nl2sql"}}}
	if _, err := engine.Validate(dag); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
