package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrSQLNotReadOnly marks statements rejected by the read-only guard.
var ErrSQLNotReadOnly = errors.New("sql guard: only read-only SELECT statements are allowed")

// forbiddenSQLKeywords are statement/DDL keywords that must never appear in a
// guarded query. Matching is word-boundary based, so identifiers such as
// updated_at do not trip "update".
var forbiddenSQLKeywords = []string{
	"insert", "update", "delete", "merge", "upsert",
	"drop", "alter", "create", "truncate", "rename",
	"grant", "revoke", "copy", "vacuum", "analyze", "reindex",
	"call", "do", "execute", "commit", "rollback", "savepoint",
	"into", "lock",
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlKeywordRules = func() []struct {
		word string
		re   *regexp.Regexp
	} {
		out := make([]struct {
			word string
			re   *regexp.Regexp
		}, 0, len(forbiddenSQLKeywords))
		for _, word := range forbiddenSQLKeywords {
			out = append(out, struct {
				word string
				re   *regexp.Regexp
			}{word: word, re: regexp.MustCompile(`(?i)\b` + word + `\b`)})
		}
		return out
	}()
)

// ValidateReadOnlySQL is the nl2sql safety gate. It accepts a single SELECT (or
// WITH ... SELECT) statement, strips comments and a trailing semicolon, and
// rejects everything else, including stacked statements, SELECT INTO and any
// DDL/DML keyword.
func ValidateReadOnlySQL(raw string) (string, error) {
	statement := strings.TrimSpace(raw)
	if statement == "" {
		return "", fmt.Errorf("%w: empty statement", ErrSQLNotReadOnly)
	}
	statement = sqlBlockComment.ReplaceAllString(statement, " ")
	statement = sqlLineComment.ReplaceAllString(statement, " ")
	statement = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(statement), ";"))
	if statement == "" {
		return "", fmt.Errorf("%w: empty statement", ErrSQLNotReadOnly)
	}
	if strings.Contains(statement, ";") {
		return "", fmt.Errorf("%w: multiple statements are not allowed", ErrSQLNotReadOnly)
	}

	lowered := strings.ToLower(statement)
	if !strings.HasPrefix(lowered, "select") && !strings.HasPrefix(lowered, "with") {
		return "", fmt.Errorf("%w: statement must start with SELECT or WITH", ErrSQLNotReadOnly)
	}
	for _, rule := range sqlKeywordRules {
		if rule.re.MatchString(statement) {
			return "", fmt.Errorf("%w: keyword %q is not allowed", ErrSQLNotReadOnly, strings.ToUpper(rule.word))
		}
	}
	return statement, nil
}

// SQLGenerator turns a natural-language question into a SQL statement.
type SQLGenerator interface {
	Generate(ctx context.Context, question string) (string, error)
}

const nl2sqlSystemPrompt = "你是 PostgreSQL SQL 生成器。根据给定的表结构和用户问题生成一条只读 SELECT 查询。" +
	"只输出 SQL 本身，不要解释，不要输出 Markdown 代码块，禁止任何写操作。"

// HTTPNL2SQLGenerator calls an OpenAI-compatible chat completion endpoint.
type HTTPNL2SQLGenerator struct {
	baseURL string
	apiKey  string
	model   string
	schema  string
	timeout time.Duration
	client  *http.Client
}

func NewHTTPNL2SQLGenerator(baseURL, apiKey, model, schema string, timeout time.Duration) *HTTPNL2SQLGenerator {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if strings.TrimSpace(model) == "" {
		model = "gpt-4o-mini"
	}
	return &HTTPNL2SQLGenerator{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		schema:  strings.TrimSpace(schema),
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

func (g *HTTPNL2SQLGenerator) Generate(ctx context.Context, question string) (string, error) {
	if g.baseURL == "" || g.apiKey == "" {
		return "", fmt.Errorf("nl2sql: LLM is not configured")
	}
	if strings.TrimSpace(question) == "" {
		return "", fmt.Errorf("nl2sql: question is empty")
	}
	system := nl2sqlSystemPrompt
	if g.schema != "" {
		system += "\n\n可查询表结构：\n" + g.schema
	}
	payload, err := json.Marshal(map[string]any{
		"model":       g.model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": question},
		},
	})
	if err != nil {
		return "", fmt.Errorf("nl2sql: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("nl2sql: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("nl2sql: request llm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("nl2sql: llm HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("nl2sql: decode llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("nl2sql: llm returned no choices")
	}
	statement := stripCodeFence(out.Choices[0].Message.Content)
	if statement == "" {
		return "", fmt.Errorf("nl2sql: llm returned empty sql")
	}
	return statement, nil
}

func stripCodeFence(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return ""
	}
	lines = lines[1:]
	if last := strings.TrimSpace(lines[len(lines)-1]); last == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// NL2SQLNode answers structured questions by running a guarded, read-only SQL
// query. The statement either comes from config (sql / sql_from) or is
// generated from config.question by the configured LLM.
type NL2SQLNode struct {
	db        *sql.DB
	generator SQLGenerator
	maxRows   int
	timeout   time.Duration
}

type NL2SQLOption func(*NL2SQLNode)

// WithNL2SQLDB attaches the database the node executes queries against.
func WithNL2SQLDB(db *sql.DB) NL2SQLOption {
	return func(n *NL2SQLNode) { n.db = db }
}

// WithNL2SQLGenerator enables question -> SQL generation.
func WithNL2SQLGenerator(g SQLGenerator) NL2SQLOption {
	return func(n *NL2SQLNode) { n.generator = g }
}

// WithNL2SQLMaxRows caps how many rows a single run may return.
func WithNL2SQLMaxRows(rows int) NL2SQLOption {
	return func(n *NL2SQLNode) {
		if rows > 0 {
			n.maxRows = rows
		}
	}
}

// WithNL2SQLTimeout bounds the query execution time.
func WithNL2SQLTimeout(d time.Duration) NL2SQLOption {
	return func(n *NL2SQLNode) {
		if d > 0 {
			n.timeout = d
		}
	}
}

func NewNL2SQLNode(opts ...NL2SQLOption) *NL2SQLNode {
	n := &NL2SQLNode{maxRows: 200, timeout: 5 * time.Second}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

func (n *NL2SQLNode) Type() string { return "nl2sql" }

// OpenNL2SQLDB opens and verifies the read-only query database.
func OpenNL2SQLDB(ctx context.Context, dsn string) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("nl2sql: database dsn is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("nl2sql: open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("nl2sql: ping database: %w", err)
	}
	return db, nil
}

func (n *NL2SQLNode) Run(ctx context.Context, ec *ExecutionContext, config map[string]any) (map[string]any, error) {
	statement := configString(config, "sql")
	if statement == "" {
		if ref := configString(config, "sql_from"); ref != "" {
			statement = contextString(ec, ref)
		}
	}
	question := configString(config, "question")
	if question == "" {
		if ref := configString(config, "question_from"); ref != "" {
			question = contextString(ec, ref)
		}
	}

	generated := false
	if strings.TrimSpace(statement) == "" && strings.TrimSpace(question) != "" {
		if n.generator == nil {
			return nil, fmt.Errorf("nl2sql: config.sql is required when no LLM is configured; ask a DBA to enable NL2SQL_LLM_*")
		}
		sqlText, err := n.generator.Generate(ctx, question)
		if err != nil {
			return nil, fmt.Errorf("nl2sql: generate sql: %w", err)
		}
		statement = sqlText
		generated = true
	}
	if strings.TrimSpace(statement) == "" {
		return nil, fmt.Errorf("nl2sql: config.sql, config.sql_from or config.question is required")
	}

	guarded, err := ValidateReadOnlySQL(statement)
	if err != nil {
		return nil, err
	}
	if n.db == nil {
		return nil, fmt.Errorf("nl2sql: database is not configured (set NL2SQL_DATABASE_URL)")
	}

	// Wrapping enforces the row cap without trusting the generated LIMIT.
	query := fmt.Sprintf("SELECT * FROM (%s) AS nl2sql_query LIMIT %d", guarded, n.maxRows)
	queryCtx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	rows, err := n.db.QueryContext(queryCtx, query)
	if err != nil {
		return nil, fmt.Errorf("nl2sql: query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("nl2sql: read columns: %w", err)
	}
	outRows := make([][]any, 0, n.maxRows)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("nl2sql: scan row: %w", err)
		}
		for i, value := range values {
			values[i] = normalizeSQLValue(value)
		}
		outRows = append(outRows, values)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("nl2sql: iterate rows: %w", err)
	}

	out := map[string]any{
		"sql":       guarded,
		"columns":   columns,
		"rows":      outRows,
		"row_count": len(outRows),
		"generated": generated,
	}
	if question != "" {
		out["question"] = question
	}
	return out, nil
}

func normalizeSQLValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return typed
	}
}

func configString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	switch value := config[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(value))
	}
}

func contextString(ec *ExecutionContext, key string) string {
	if ec == nil || key == "" {
		return ""
	}
	value, ok := ec.Get(key)
	if !ok {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
