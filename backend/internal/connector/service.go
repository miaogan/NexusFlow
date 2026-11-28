package connector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxBatchesPerSync = 10

// Registrar turns connector items into ordinary ingestion tasks. The signature
// stays primitive so ingestion does not need to import this package.
type Registrar interface {
	RegisterContent(
		ctx context.Context,
		tenantID, kbID, connectorID, externalID, fileName, fileType string,
		content []byte,
	) (taskID string, created bool, err error)
}

// Service manages connector definitions and executes incremental syncs.
type Service struct {
	store     Store
	sources   map[Type]Source
	registrar Registrar
	logger    *slog.Logger
}

func NewService(store Store, registrar Registrar, logger *slog.Logger, sources ...Source) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	byType := make(map[Type]Source, len(sources))
	for _, source := range sources {
		byType[source.Type()] = source
	}
	return &Service{store: store, sources: byType, registrar: registrar, logger: logger}
}

func (s *Service) List(ctx context.Context, tenantID, kbID string) ([]Spec, error) {
	return s.store.List(ctx, tenantID, kbID)
}

// All returns every connector; the scheduler runs due ones across tenants.
func (s *Service) All(ctx context.Context) ([]Spec, error) {
	return s.store.ListAll(ctx)
}

// ClaimSchedule deduplicates one scheduled run across replicas.
func (s *Service) ClaimSchedule(ctx context.Context, id string, minute time.Time) (bool, error) {
	return s.store.ClaimSchedule(ctx, id, minute)
}

// PruneScheduleClaims drops old claims so the table stays small.
func (s *Service) PruneScheduleClaims(ctx context.Context, before time.Time) error {
	return s.store.PruneScheduleClaims(ctx, before)
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (Spec, bool, error) {
	return s.store.Get(ctx, tenantID, id)
}

func (s *Service) Create(ctx context.Context, tenantID string, spec Spec) (Spec, error) {
	spec.TenantID = tenantID
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" || len(spec.Name) > 128 {
		return Spec{}, fmt.Errorf("connector: name must be 1-128 characters")
	}
	if strings.TrimSpace(spec.KBID) == "" {
		return Spec{}, fmt.Errorf("connector: kb_id is required")
	}
	source, ok := s.sources[spec.Type]
	if !ok {
		return Spec{}, fmt.Errorf("connector: unsupported type %q", spec.Type)
	}
	if err := source.Validate(spec.Config); err != nil {
		return Spec{}, err
	}
	if strings.TrimSpace(spec.ScheduleCron) != "" {
		if _, err := parseCron(spec.ScheduleCron); err != nil {
			return Spec{}, err
		}
	}
	now := time.Now()
	if spec.ID == "" {
		spec.ID = uuid.NewString()
	}
	spec.Status = StatusIdle
	spec.SyncCursor = map[string]string{}
	spec.CreatedAt = now
	spec.UpdatedAt = now
	return s.store.Create(ctx, spec)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	return s.store.Delete(ctx, tenantID, id)
}

// Sync pulls up to maxBatchesPerSync batches, ingesting every document. The
// cursor is only persisted together with the final status, so a failed pull
// leaves the previous cursor intact for the next attempt.
func (s *Service) Sync(ctx context.Context, tenantID, id string) (Result, error) {
	spec, ok, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrNotFound
	}
	if spec.Status == StatusDisabled {
		return Result{}, fmt.Errorf("connector %s is disabled", id)
	}
	source, ok := s.sources[spec.Type]
	if !ok {
		return Result{}, fmt.Errorf("connector: unsupported type %q", spec.Type)
	}
	if s.registrar == nil {
		return Result{}, fmt.Errorf("connector: ingestion registrar is not configured")
	}

	spec.Status = StatusSyncing
	spec.LastError = ""
	spec.UpdatedAt = time.Now()
	if err := s.store.Update(ctx, spec); err != nil {
		return Result{}, err
	}

	result := Result{ConnectorID: spec.ID}
	cursor := spec.SyncCursor
	if cursor == nil {
		cursor = map[string]string{}
	}
	var runErr error
	for batchNo := 0; batchNo < maxBatchesPerSync; batchNo++ {
		batch, pullErr := source.Pull(ctx, spec.Config, cursor)
		if pullErr != nil {
			runErr = pullErr
			break
		}
		for _, item := range batch.Items {
			taskID, created, registerErr := s.registrar.RegisterContent(
				ctx, spec.TenantID, spec.KBID, spec.ID,
				item.ExternalID, item.FileName, item.FileType, item.Content,
			)
			if registerErr != nil {
				result.Failed++
				result.Errors = append(result.Errors, truncate(registerErr.Error(), 200))
				continue
			}
			if created {
				result.Created++
				result.TaskIDs = append(result.TaskIDs, taskID)
			} else {
				result.Skipped++
			}
		}
		if batch.NextCursor != nil {
			cursor = batch.NextCursor
		}
		if batch.Done || len(batch.Items) == 0 {
			break
		}
	}

	now := time.Now()
	spec.SyncCursor = cursor
	spec.UpdatedAt = now
	if runErr != nil {
		spec.Status = StatusError
		spec.LastError = truncate(runErr.Error(), 500)
	} else {
		spec.Status = StatusIdle
		spec.LastSyncAt = now
	}
	if err := s.store.Update(ctx, spec); err != nil && runErr == nil {
		return result, err
	}
	s.logger.Info("connector sync finished", "connector", spec.ID, "type", spec.Type,
		"created", result.Created, "skipped", result.Skipped, "failed", result.Failed)
	return result, runErr
}

// MaskConfig hides credentials before a connector leaves the service.
func MaskConfig(cfg Config) Config {
	masked := cfg
	if masked.AccessKey != "" {
		masked.AccessKey = "***"
	}
	if masked.SecretKey != "" {
		masked.SecretKey = "***"
	}
	if masked.DSN != "" {
		masked.DSN = "***"
	}
	return masked
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
