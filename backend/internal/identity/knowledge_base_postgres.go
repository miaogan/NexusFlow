package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const knowledgeBaseColumns = `id, tenant_id, name, description, created_at, updated_at`

func (s *PostgresStore) CreateKnowledgeBase(ctx context.Context, kb KnowledgeBase) (KnowledgeBase, error) {
	kb.TenantID = tenantOr(kb.TenantID)
	kb.Name = strings.TrimSpace(kb.Name)
	if kb.Name == "" {
		return KnowledgeBase{}, ErrKnowledgeBaseName
	}
	if kb.ID == "" {
		kb.ID = uuid.NewString()
	}
	now := time.Now()
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	kb.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_knowledge_bases
		 (id, tenant_id, name, description, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		kb.ID, kb.TenantID, kb.Name, kb.Description, kb.CreatedAt, kb.UpdatedAt,
	)
	if isUniqueViolation(err) {
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	if err != nil {
		return KnowledgeBase{}, err
	}
	return kb, nil
}

func (s *PostgresStore) GetKnowledgeBase(ctx context.Context, tenantID, id string) (KnowledgeBase, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+knowledgeBaseColumns+` FROM identity_knowledge_bases
		 WHERE tenant_id = $1 AND id = $2`, tenantOr(tenantID), id)
	var kb KnowledgeBase
	if err := row.Scan(&kb.ID, &kb.TenantID, &kb.Name, &kb.Description, &kb.CreatedAt, &kb.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return KnowledgeBase{}, false, nil
	} else if err != nil {
		return KnowledgeBase{}, false, err
	}
	return kb, true, nil
}

func (s *PostgresStore) ListKnowledgeBases(ctx context.Context, tenantID string) ([]KnowledgeBase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+knowledgeBaseColumns+` FROM identity_knowledge_bases
		 WHERE tenant_id = $1 ORDER BY created_at ASC`, tenantOr(tenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]KnowledgeBase, 0)
	for rows.Next() {
		var kb KnowledgeBase
		if err := rows.Scan(&kb.ID, &kb.TenantID, &kb.Name, &kb.Description, &kb.CreatedAt, &kb.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, kb)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateKnowledgeBase(ctx context.Context, patch KnowledgeBase) (KnowledgeBase, error) {
	tenantID := tenantOr(patch.TenantID)
	existing, ok, err := s.GetKnowledgeBase(ctx, tenantID, patch.ID)
	if err != nil {
		return KnowledgeBase{}, err
	}
	if !ok {
		return KnowledgeBase{}, ErrKnowledgeBaseNotFound
	}
	if name := strings.TrimSpace(patch.Name); name != "" {
		existing.Name = name
	}
	existing.Description = patch.Description
	existing.UpdatedAt = time.Now()
	_, err = s.db.ExecContext(ctx,
		`UPDATE identity_knowledge_bases
		 SET name = $1, description = $2, updated_at = $3
		 WHERE tenant_id = $4 AND id = $5`,
		existing.Name, existing.Description, existing.UpdatedAt, tenantID, existing.ID,
	)
	if isUniqueViolation(err) {
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	if err != nil {
		return KnowledgeBase{}, err
	}
	return existing, nil
}

func (s *PostgresStore) DeleteKnowledgeBase(ctx context.Context, tenantID, id string) error {
	if id == DefaultKnowledgeBaseID {
		return ErrDefaultKnowledgeBase
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM identity_knowledge_bases WHERE tenant_id = $1 AND id = $2`, tenantOr(tenantID), id)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrKnowledgeBaseNotFound
	}
	return nil
}
