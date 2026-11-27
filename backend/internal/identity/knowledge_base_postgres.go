package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const knowledgeBaseColumns = `id, tenant_id, name, description, embedding_model,
	chunk_strategy, status, created_by, created_at, updated_at`

const kbPermissionColumns = `id, tenant_id, kb_id, subject_type, subject_id, permission, created_at`

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
	if kb.Status == "" {
		kb.Status = KBStatusActive
	}
	if kb.EmbeddingModel == "" {
		kb.EmbeddingModel = "default"
	}
	if kb.CreatedBy == "" {
		kb.CreatedBy = "system"
	}
	kb.UpdatedAt = now
	strategy, err := marshalChunkStrategy(kb.ChunkStrategy)
	if err != nil {
		return KnowledgeBase{}, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO knowledge_bases
		 (id, tenant_id, name, description, embedding_model, chunk_strategy, status, created_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		kb.ID, kb.TenantID, kb.Name, kb.Description, kb.EmbeddingModel,
		strategy, kb.Status, kb.CreatedBy, kb.CreatedAt, kb.UpdatedAt,
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
		`SELECT `+knowledgeBaseColumns+` FROM knowledge_bases
		 WHERE tenant_id = $1 AND id = $2`, tenantOr(tenantID), id)
	kb, err := scanKnowledgeBase(row)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeBase{}, false, nil
	}
	if err != nil {
		return KnowledgeBase{}, false, err
	}
	return kb, true, nil
}

func (s *PostgresStore) ListKnowledgeBases(ctx context.Context, tenantID string) ([]KnowledgeBase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+knowledgeBaseColumns+` FROM knowledge_bases
		 WHERE tenant_id = $1 ORDER BY created_at ASC`, tenantOr(tenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]KnowledgeBase, 0)
	for rows.Next() {
		kb, err := scanKnowledgeBase(rows)
		if err != nil {
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
		`UPDATE knowledge_bases
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
		`DELETE FROM knowledge_bases WHERE tenant_id = $1 AND id = $2`, tenantOr(tenantID), id)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrKnowledgeBaseNotFound
	}
	return nil
}

// ---------- kb-scoped permissions ----------

func (s *PostgresStore) UpsertKnowledgeBasePermission(ctx context.Context, perm KnowledgeBasePermission) (KnowledgeBasePermission, error) {
	perm.TenantID = tenantOr(perm.TenantID)
	if perm.ID == "" {
		perm.ID = uuid.NewString()
	}
	if perm.CreatedAt.IsZero() {
		perm.CreatedAt = time.Now()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO kb_permissions
		 (id, tenant_id, kb_id, subject_type, subject_id, permission, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (kb_id, subject_type, subject_id) DO UPDATE
		 SET permission = EXCLUDED.permission
		 RETURNING `+kbPermissionColumns,
		perm.ID, perm.TenantID, perm.KBID, perm.SubjectType, perm.SubjectID, perm.Permission, perm.CreatedAt,
	)
	out, err := scanKBPermission(row)
	if err != nil {
		return KnowledgeBasePermission{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteKnowledgeBasePermission(ctx context.Context, tenantID, kbID, subjectType, subjectID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM kb_permissions
		 WHERE tenant_id = $1 AND kb_id = $2 AND subject_type = $3 AND subject_id = $4`,
		tenantOr(tenantID), kbID, subjectType, subjectID)
	return err
}

func (s *PostgresStore) ListKnowledgeBasePermissions(ctx context.Context, tenantID, kbID string) ([]KnowledgeBasePermission, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+kbPermissionColumns+` FROM kb_permissions
		 WHERE tenant_id = $1 AND kb_id = $2
		 ORDER BY subject_type ASC, subject_id ASC`, tenantOr(tenantID), kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]KnowledgeBasePermission, 0)
	for rows.Next() {
		perm, err := scanKBPermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, perm)
	}
	return out, rows.Err()
}

func scanKnowledgeBase(scanner rowScanner) (KnowledgeBase, error) {
	var (
		kb       KnowledgeBase
		strategy []byte
	)
	if err := scanner.Scan(
		&kb.ID, &kb.TenantID, &kb.Name, &kb.Description, &kb.EmbeddingModel,
		&strategy, &kb.Status, &kb.CreatedBy, &kb.CreatedAt, &kb.UpdatedAt,
	); err != nil {
		return KnowledgeBase{}, err
	}
	if len(strategy) > 0 {
		if err := json.Unmarshal(strategy, &kb.ChunkStrategy); err != nil {
			return KnowledgeBase{}, err
		}
	}
	return kb, nil
}

func scanKBPermission(scanner rowScanner) (KnowledgeBasePermission, error) {
	var perm KnowledgeBasePermission
	if err := scanner.Scan(
		&perm.ID, &perm.TenantID, &perm.KBID, &perm.SubjectType,
		&perm.SubjectID, &perm.Permission, &perm.CreatedAt,
	); err != nil {
		return KnowledgeBasePermission{}, err
	}
	return perm, nil
}
