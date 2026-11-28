package connector

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCronExpressionMatching(t *testing.T) {
	schedule, err := parseCron("*/15 9-18 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	friday := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	if !schedule.Match(friday) {
		t.Fatal("expected Friday 09:30 to match")
	}
	if schedule.Match(friday.Add(10 * time.Minute)) {
		t.Fatal("09:40 is not a multiple of 15")
	}
	if schedule.Match(time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)) {
		t.Fatal("20:00 is outside 9-18")
	}
	if schedule.Match(time.Date(2026, 11, 29, 9, 30, 0, 0, time.UTC)) {
		t.Fatal("Sunday is not in 1-5")
	}
}

func TestCronDayOfMonthOrWeekday(t *testing.T) {
	schedule, err := parseCron("0 0 1 * 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Restricted day-of-month and weekday fire on either match.
	cases := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), true},  // 1st (Tuesday)
		{time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), true}, // Monday
		{time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC), false}, // Wednesday, not 1st
	}
	for _, tc := range cases {
		if got := schedule.Match(tc.at); got != tc.want {
			t.Fatalf("Match(%s) = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestCronRejectsInvalidExpressions(t *testing.T) {
	for _, expr := range []string{
		"* * *",
		"61 * * * *",
		"*/0 * * * *",
		"* * * * 8",
		"0 0 0 * *",
	} {
		if _, err := parseCron(expr); err == nil {
			t.Fatalf("expected %q to be rejected", expr)
		}
	}
}

func TestServiceCreateRejectsInvalidSchedule(t *testing.T) {
	svc, _ := newTestService(t, &fakeSource{kind: TypeWeb}, &fakeRegistrar{})
	_, err := svc.Create(context.Background(), "t1", Spec{
		Name: "docs", KBID: "kb1", Type: TypeWeb,
		Config:       Config{URLs: []string{"http://example.com"}},
		ScheduleCron: "not-a-cron",
	})
	if err == nil {
		t.Fatal("expected invalid schedule_cron to be rejected")
	}
}

func TestSchedulerSyncsDueConnectorOncePerMinute(t *testing.T) {
	source := &fakeSource{kind: TypeWeb, batches: []Batch{{
		Items: []Item{{ExternalID: "a", FileName: "a.html", FileType: "html", Content: []byte("a")}},
		Done:  true,
	}}}
	svc, store := newTestService(t, source, &fakeRegistrar{existing: map[string]bool{}})
	ctx := context.Background()

	due, err := svc.Create(ctx, "t1", Spec{
		Name: "due", KBID: "kb1", Type: TypeWeb,
		Config:       Config{URLs: []string{"http://example.com"}},
		ScheduleCron: "* * * * *",
	})
	if err != nil {
		t.Fatalf("create due: %v", err)
	}
	notDue, err := svc.Create(ctx, "t1", Spec{
		Name: "not-due", KBID: "kb1", Type: TypeWeb,
		Config:       Config{URLs: []string{"http://example.com"}},
		ScheduleCron: "0 0 1 1 *",
	})
	if err != nil {
		t.Fatalf("create not-due: %v", err)
	}

	scheduler := NewScheduler(svc, discardLogger(), time.Minute, time.Minute)
	now := time.Date(2026, 11, 28, 10, 30, 0, 0, time.UTC)
	scheduler.runOnce(ctx, now)

	if source.calls != 1 {
		t.Fatalf("pulls = %d, want 1", source.calls)
	}
	if saved, _, _ := store.Get(ctx, "t1", due.ID); saved.LastSyncAt.IsZero() {
		t.Fatalf("due connector not synced: %+v", saved)
	}
	if saved, _, _ := store.Get(ctx, "t1", notDue.ID); !saved.LastSyncAt.IsZero() {
		t.Fatalf("not-due connector should not run: %+v", saved)
	}

	// A second evaluation within the same minute must not refire.
	scheduler.runOnce(ctx, now.Add(20*time.Second))
	if source.calls != 1 {
		t.Fatalf("pulls in same minute = %d, want 1", source.calls)
	}

	// The next minute fires again.
	scheduler.runOnce(ctx, now.Add(time.Minute))
	if source.calls != 2 {
		t.Fatalf("pulls after next minute = %d, want 2", source.calls)
	}
}
