package feedback

import "context"

// NegativeCounter aggregates negative ratings per cited chunk for the retrieval
// down-ranking stage. It paginates the feedback listing so counts reflect the
// tenant's full history rather than only the newest page.
type NegativeCounter struct {
	Store Store
	// PageSize bounds each List call (default and maximum: 200).
	PageSize int
	// MaxPages caps scanning (default 50 pages, i.e. 10k entries).
	MaxPages int
}

// NewNegativeCounter adapts a Store into the query pipeline's counter contract
// (query.NegativeFeedbackCounter); the method sets match structurally.
func NewNegativeCounter(s Store) NegativeCounter { return NegativeCounter{Store: s} }

// NegativeCounts counts, per cited chunk, how many negative feedback records
// reference it within the tenant. Chunks without negative feedback are omitted.
func (c NegativeCounter) NegativeCounts(ctx context.Context, tenantID string, chunkIDs []string) (map[string]int, error) {
	if c.Store == nil || len(chunkIDs) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(chunkIDs))
	for _, id := range chunkIDs {
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	pageSize := c.PageSize
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 200
	}
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}

	counts := make(map[string]int)
	offset := 0
	for page := 0; page < maxPages; page++ {
		entries, err := c.Store.List(ctx, Filter{
			TenantID:     tenantID,
			NegativeOnly: true,
			Limit:        pageSize,
			Offset:       offset,
		})
		if err != nil {
			return counts, err
		}
		for _, e := range entries {
			seen := make(map[string]struct{}, len(e.CitedChunkIDs))
			for _, id := range e.CitedChunkIDs {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				if _, ok := wanted[id]; ok {
					counts[id]++
				}
			}
		}
		if len(entries) < pageSize {
			break
		}
		offset += len(entries)
	}
	return counts, nil
}
