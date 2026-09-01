package backup

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAssignRetentionTag(t *testing.T) {
	cases := []struct {
		name string
		date time.Time
		want string
	}{
		// 2026-08-31 is a Monday → plain daily.
		{"mid-month monday", time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC), types.BackupRetentionDaily},
		// 2026-08-30 is a Sunday → weekly.
		{"sunday", time.Date(2026, 8, 30, 3, 0, 0, 0, time.UTC), types.BackupRetentionWeekly},
		// 2026-08-31 is the month's last day → monthly (outranks weekly).
		{"month end", time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC), types.BackupRetentionMonthly},
		// 2026-09-30 is also a month end.
		{"september end", time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC), types.BackupRetentionMonthly},
	}
	for _, tc := range cases {
		if got := assignRetentionTag(tc.date); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestSelectExpiredGFS(t *testing.T) {
	now := time.Now().UTC()
	finished := func(daysAgo int) *time.Time {
		t := now.AddDate(0, 0, -daysAgo)
		return &t
	}
	rec := func(id string, daysAgo int, tag string) *types.BackupRecord {
		return &types.BackupRecord{
			ID: id, Status: types.BackupStatusSucceeded,
			StartedAt: now.AddDate(0, 0, -daysAgo), FinishedAt: finished(daysAgo),
			RetentionTag: tag,
		}
	}

	records := []*types.BackupRecord{
		rec("fresh-daily", 1, types.BackupRetentionDaily),      // 1 day old, keep
		rec("old-daily", 10, types.BackupRetentionDaily),       // 10 days > 7, expire
		rec("old-weekly", 30, types.BackupRetentionWeekly),     // 30 days > 28, expire
		rec("kept-weekly", 20, types.BackupRetentionWeekly),    // 20 days < 28, keep
		rec("kept-monthly", 100, types.BackupRetentionMonthly), // 100 days < 180, keep
		rec("old-monthly", 200, types.BackupRetentionMonthly),  // 200 days > 180, expire
		// Failed/running records are never policy-pruned.
		{ID: "failed", Status: types.BackupStatusFailed, StartedAt: now.AddDate(0, 0, -400)},
		{ID: "running", Status: types.BackupStatusRunning, StartedAt: now.AddDate(0, 0, -400)},
	}

	expired := SelectExpired(records, 7, 4, 6)
	got := map[string]bool{}
	for _, r := range expired {
		got[r.ID] = true
	}
	for _, want := range []string{"old-daily", "old-weekly", "old-monthly"} {
		if !got[want] {
			t.Errorf("expected %s to expire; got %v", want, expiredIDs(expired))
		}
	}
	for _, notWant := range []string{"fresh-daily", "kept-weekly", "kept-monthly", "failed", "running"} {
		if got[notWant] {
			t.Errorf("expected %s to survive; got %v", notWant, expiredIDs(expired))
		}
	}

	// Tier disabled (0) → that tier's records survive forever.
	expired = SelectExpired(records, 0, 4, 6)
	for _, r := range expired {
		if r.RetentionTag == types.BackupRetentionDaily {
			t.Errorf("daily tier disabled but %s expired", r.ID)
		}
	}
}

func expiredIDs(records []*types.BackupRecord) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.ID)
	}
	return out
}
