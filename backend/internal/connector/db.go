package connector

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	sqlguard "github.com/nexusflow/nexusflow/backend/internal/workflow"
)

const (
	maxDBRows      = 200
	defaultDBRows  = 50
	maxRowValueLen = 1000
	maxRowColumns  = 40
)

// DBSource turns rows of a read-only SELECT into Markdown documents. The
// cursor is an OFFSET, so the query must have a stable ORDER BY.
type DBSource struct{}

func (DBSource) Type() Type { return TypeDB }

func (DBSource) Validate(cfg Config) error {
	if strings.TrimSpace(cfg.DSN) == "" {
		return fmt.Errorf("db_query: dsn is required")
	}
	if strings.TrimSpace(cfg.SQL) == "" {
		return fmt.Errorf("db_query: sql is required")
	}
	if _, err := sqlguard.ValidateReadOnlySQL(cfg.SQL); err != nil {
		return fmt.Errorf("db_query: %w", err)
	}
	if cfg.BatchSize < 0 || cfg.BatchSize > maxDBRows {
		return fmt.Errorf("db_query: batch_size must be between 1 and %d", maxDBRows)
	}
	return nil
}

func (DBSource) Pull(ctx context.Context, cfg Config, cursor map[string]string) (Batch, error) {
	statement, err := sqlguard.ValidateReadOnlySQL(cfg.SQL)
	if err != nil {
		return Batch{}, fmt.Errorf("db_query: %w", err)
	}
	limit := cfg.BatchSize
	if limit <= 0 {
		limit = defaultDBRows
	}
	offset, _ := strconv.Atoi(cursor["offset"])
	if offset < 0 {
		offset = 0
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return Batch{}, fmt.Errorf("db_query: open: %w", err)
	}
	defer db.Close()

	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wrapped := fmt.Sprintf("SELECT * FROM (%s) AS connector_src LIMIT %d OFFSET %d", statement, limit, offset)
	rows, err := db.QueryContext(queryCtx, wrapped)
	if err != nil {
		return Batch{}, fmt.Errorf("db_query: query: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return Batch{}, fmt.Errorf("db_query: columns: %w", err)
	}
	batch := Batch{NextCursor: map[string]string{"offset": strconv.Itoa(offset)}, Done: true}
	for rows.Next() {
		if len(batch.Items) >= limit {
			break
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return Batch{}, fmt.Errorf("db_query: scan: %w", err)
		}
		rowNumber := offset + len(batch.Items) + 1
		externalID := rowExternalID(columns, values, cfg.IDColumn, rowNumber)
		batch.Items = append(batch.Items, Item{
			ExternalID: externalID,
			FileName:   "db-" + safeSlug(externalID) + ".md",
			FileType:   "md",
			Content:    []byte(renderRow(externalID, columns, values)),
		})
	}
	if err := rows.Err(); err != nil {
		return Batch{}, fmt.Errorf("db_query: rows: %w", err)
	}
	batch.NextCursor["offset"] = strconv.Itoa(offset + len(batch.Items))
	if len(batch.Items) >= limit {
		batch.Done = false
	}
	return batch, nil
}

func rowExternalID(columns []string, values []any, idColumn string, fallback int) string {
	if idColumn != "" {
		for i, column := range columns {
			if column == idColumn {
				if value := renderValue(values[i]); value != "" {
					return value
				}
			}
		}
	}
	return strconv.Itoa(fallback)
}

// renderRow turns one row into a small Markdown document so the normal
// MinerU → LlamaIndex pipeline can chunk and index it.
func renderRow(id string, columns []string, values []any) string {
	var builder strings.Builder
	builder.WriteString("# Database row ")
	builder.WriteString(id)
	builder.WriteString("\n\n")
	limit := len(columns)
	if limit > maxRowColumns {
		limit = maxRowColumns
	}
	for i := 0; i < limit; i++ {
		builder.WriteString("- ")
		builder.WriteString(columns[i])
		builder.WriteString(": ")
		builder.WriteString(renderValue(values[i]))
		builder.WriteString("\n")
	}
	return builder.String()
}

func renderValue(value any) string {
	var text string
	switch typed := value.(type) {
	case nil:
		return ""
	case []byte:
		text = string(typed)
	case time.Time:
		text = typed.UTC().Format(time.RFC3339)
	default:
		text = fmt.Sprint(typed)
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxRowValueLen {
		text = text[:maxRowValueLen]
	}
	return text
}

func safeSlug(value string) string {
	slug := unsafeNameChars.ReplaceAllString(value, "-")
	slug = strings.Trim(slug, "-.")
	if slug == "" {
		slug = "row"
	}
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return slug
}
