package connector

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	maxS3Objects     = 200
	defaultS3Objects = 50
	maxS3ObjectBytes = 20 << 20
)

// S3Source lists and downloads objects from an S3-compatible bucket. The cursor
// is the last key seen, so each sync continues where the previous one stopped.
type S3Source struct{}

func (S3Source) Type() Type { return TypeS3 }

func (S3Source) Validate(cfg Config) error {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return fmt.Errorf("s3_bucket: endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return fmt.Errorf("s3_bucket: bucket is required")
	}
	if (cfg.AccessKey == "") != (cfg.SecretKey == "") {
		return fmt.Errorf("s3_bucket: access_key and secret_key must be provided together")
	}
	if cfg.MaxObjects < 0 || cfg.MaxObjects > maxS3Objects {
		return fmt.Errorf("s3_bucket: max_objects must be between 1 and %d", maxS3Objects)
	}
	return nil
}

func (S3Source) Pull(ctx context.Context, cfg Config, cursor map[string]string) (Batch, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" {
			return Batch{}, fmt.Errorf("s3_bucket: invalid endpoint %q", cfg.Endpoint)
		}
		endpoint = parsed.Host
	}
	options := &minio.Options{Secure: cfg.UseSSL, Region: cfg.Region}
	if cfg.AccessKey != "" {
		options.Creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}
	client, err := minio.New(endpoint, options)
	if err != nil {
		return Batch{}, fmt.Errorf("s3_bucket: client: %w", err)
	}

	limit := cfg.MaxObjects
	if limit <= 0 {
		limit = defaultS3Objects
	}
	scanCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	lastKey := cursor["last_key"]
	batch := Batch{NextCursor: map[string]string{"last_key": lastKey}, Done: true}
	objects := client.ListObjects(scanCtx, cfg.Bucket, minio.ListObjectsOptions{
		Prefix:     cfg.Prefix,
		Recursive:  true,
		StartAfter: lastKey,
		MaxKeys:    limit,
	})
	for object := range objects {
		if object.Err != nil {
			return Batch{}, fmt.Errorf("s3_bucket: list: %w", object.Err)
		}
		if strings.HasSuffix(object.Key, "/") {
			continue
		}
		fileType, ok := fileTypeFromName(object.Key)
		if !ok {
			continue
		}
		reader, err := client.GetObject(scanCtx, cfg.Bucket, object.Key, minio.GetObjectOptions{})
		if err != nil {
			return Batch{}, fmt.Errorf("s3_bucket: get %s: %w", object.Key, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(reader, maxS3ObjectBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return Batch{}, fmt.Errorf("s3_bucket: read %s: %w", object.Key, readErr)
		}
		if closeErr != nil {
			return Batch{}, fmt.Errorf("s3_bucket: close %s: %w", object.Key, closeErr)
		}
		if len(raw) == 0 || int64(len(raw)) > maxS3ObjectBytes {
			continue
		}
		batch.Items = append(batch.Items, Item{
			ExternalID: object.Key,
			FileName:   path.Base(object.Key),
			FileType:   fileType,
			Content:    raw,
		})
		lastKey = object.Key
		if len(batch.Items) >= limit {
			batch.Done = false
			break
		}
	}
	batch.NextCursor["last_key"] = lastKey
	return batch, nil
}

func fileTypeFromName(name string) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch ext {
	case "pdf", "docx", "txt", "md", "markdown", "html", "htm":
		if ext == "markdown" {
			ext = "md"
		}
		if ext == "htm" {
			ext = "html"
		}
		return ext, true
	default:
		return "", false
	}
}
