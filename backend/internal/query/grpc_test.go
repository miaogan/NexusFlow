package query

import "testing"

func TestScopedFiltersOverrideClientIsolationFields(t *testing.T) {
	filters := scopedFilters(map[string]string{
		"tenant_id": "spoofed-tenant", "kb_id": "spoofed-kb", "file_name": "a.md",
	}, "real-tenant", "real-kb")
	if filters["tenant_id"] != "real-tenant" || filters["kb_id"] != "real-kb" {
		t.Fatalf("isolation filters were not enforced: %v", filters)
	}
	if filters["file_name"] != "a.md" {
		t.Fatalf("ordinary filter lost: %v", filters)
	}
	filters["file_name"] = "changed"
	defaults := scopedFilters(nil, "tenant", "")
	if defaults["kb_id"] != "default" {
		t.Fatalf("default kb filter = %v", defaults)
	}
}
