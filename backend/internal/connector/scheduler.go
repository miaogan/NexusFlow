package connector

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSchedulerInterval is how often cron expressions are evaluated.
	DefaultSchedulerInterval = 30 * time.Second
	// DefaultSyncTimeout bounds one scheduled sync run.
	DefaultSyncTimeout = 10 * time.Minute
)

// Scheduler runs connectors whose schedule_cron matches the current minute.
// Runs are claimed in the store, so overlapping replicas fire each connector
// at most once per minute.
type Scheduler struct {
	service  *Service
	logger   *slog.Logger
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
}

// NewScheduler builds a scheduler; zero durations fall back to the defaults.
func NewScheduler(svc *Service, logger *slog.Logger, interval, timeout time.Duration) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultSchedulerInterval
	}
	if timeout <= 0 {
		timeout = DefaultSyncTimeout
	}
	return &Scheduler{
		service:  svc,
		logger:   logger,
		interval: interval,
		timeout:  timeout,
		now:      time.Now,
	}
}

// Run evaluates schedules until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx, s.now())
		}
	}
}

// runOnce syncs every connector due at now. Syncs run sequentially so a slow
// source cannot fan out into unbounded concurrent pulls.
func (s *Scheduler) runOnce(ctx context.Context, now time.Time) {
	specs, err := s.service.All(ctx)
	if err != nil {
		s.logger.Warn("connector scheduler list failed", "error", err)
		return
	}
	minute := now.Truncate(time.Minute)
	for _, spec := range specs {
		cron := strings.TrimSpace(spec.ScheduleCron)
		if cron == "" || spec.Status == StatusDisabled || spec.Status == StatusSyncing {
			continue
		}
		schedule, err := parseCron(cron)
		if err != nil {
			s.logger.Warn("connector schedule invalid",
				"connector", spec.ID, "cron", cron, "error", err)
			continue
		}
		if !schedule.Match(now) {
			continue
		}
		claimed, err := s.service.ClaimSchedule(ctx, spec.ID, minute)
		if err != nil {
			s.logger.Warn("connector schedule claim failed", "connector", spec.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		s.syncOne(ctx, spec)
	}
	if err := s.service.PruneScheduleClaims(ctx, minute.Add(-24*time.Hour)); err != nil {
		s.logger.Warn("connector schedule prune failed", "error", err)
	}
}

func (s *Scheduler) syncOne(ctx context.Context, spec Spec) {
	syncCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	result, err := s.service.Sync(syncCtx, spec.TenantID, spec.ID)
	if err != nil {
		s.logger.Warn("scheduled connector sync failed", "connector", spec.ID, "error", err)
		return
	}
	s.logger.Info("scheduled connector sync finished", "connector", spec.ID,
		"created", result.Created, "skipped", result.Skipped, "failed", result.Failed)
}

// cronSpec is a parsed five-field cron expression: minute, hour, day of month,
// month and day of week (Sunday = 0).
type cronSpec struct {
	minutes    uint64
	hours      uint64
	days       uint64
	months     uint64
	weekdays   uint64
	dayAny     bool
	weekdayAny bool
}

func parseCron(expr string) (*cronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}
	minutes, _, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron minute: %w", err)
	}
	hours, _, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron hour: %w", err)
	}
	days, dayAny, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-month: %w", err)
	}
	months, _, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron month: %w", err)
	}
	weekdays, weekdayAny, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return nil, fmt.Errorf("cron day-of-week: %w", err)
	}
	return &cronSpec{
		minutes:    minutes,
		hours:      hours,
		days:       days,
		months:     months,
		weekdays:   weekdays,
		dayAny:     dayAny,
		weekdayAny: weekdayAny,
	}, nil
}

// parseCronField turns one field into a bitmask. any is true only for a bare
// "*", which drives the cron day-of-month/day-of-week OR rule.
func parseCronField(field string, lo, hi int) (mask uint64, any bool, err error) {
	if field == "*" {
		return cronRange(lo, hi), true, nil
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, false, fmt.Errorf("empty list item")
		}
		step := 1
		if idx := strings.IndexByte(part, '/'); idx >= 0 {
			value, convErr := strconv.Atoi(part[idx+1:])
			if convErr != nil || value <= 0 {
				return 0, false, fmt.Errorf("invalid step %q", part[idx+1:])
			}
			step = value
			part = part[:idx]
		}
		start, end := lo, hi
		switch {
		case part == "*":
			// keep the full range
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			from, fromErr := strconv.Atoi(bounds[0])
			to, toErr := strconv.Atoi(bounds[1])
			if fromErr != nil || toErr != nil {
				return 0, false, fmt.Errorf("invalid range %q", part)
			}
			start, end = from, to
		default:
			value, convErr := strconv.Atoi(part)
			if convErr != nil {
				return 0, false, fmt.Errorf("invalid value %q", part)
			}
			start, end = value, value
		}
		if start < lo || end > hi || start > end {
			return 0, false, fmt.Errorf("value %q out of range %d-%d", part, lo, hi)
		}
		for value := start; value <= end; value += step {
			mask |= 1 << uint(value)
		}
	}
	return mask, false, nil
}

func cronRange(lo, hi int) uint64 {
	var mask uint64
	for value := lo; value <= hi; value++ {
		mask |= 1 << uint(value)
	}
	return mask
}

// Match reports whether the expression fires at the given minute.
func (c *cronSpec) Match(t time.Time) bool {
	if c.minutes&(1<<uint(t.Minute())) == 0 ||
		c.hours&(1<<uint(t.Hour())) == 0 ||
		c.months&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	dom := c.days&(1<<uint(t.Day())) != 0
	dow := c.weekdays&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case c.dayAny && c.weekdayAny:
		return true
	case c.dayAny:
		return dow
	case c.weekdayAny:
		return dom
	default:
		// Standard cron: restricted day-of-month and weekday use OR.
		return dom || dow
	}
}
