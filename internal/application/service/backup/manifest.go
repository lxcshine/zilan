package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// manifestRelPath is the snapshot-root manifest key.
const manifestRelPath = "manifest.json"

// writeManifest serializes and stores the manifest at the given snapshot
// path. The manifest itself stays plaintext (PRD §4.4 — it must remain
// readable to enumerate damage); only the metadata blobs are sealed.
func writeManifest(ctx context.Context, store BackupStorage, relPath string, m *types.BackupManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return store.Put(ctx, relPath, strings.NewReader(string(data)))
}

// readManifest loads and parses a snapshot manifest.
func readManifest(ctx context.Context, store BackupStorage, relPath string) (*types.BackupManifest, error) {
	r, err := store.Get(ctx, relPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer r.Close()
	var m types.BackupManifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// assignRetentionTag picks the GFS tier for a snapshot per the PRD's
// simplified scheme: the last daily of a ISO week becomes that week's
// weekly, the last daily of a month becomes that month's monthly. A
// monthly outranks a weekly, which outranks a plain daily.
func assignRetentionTag(recordTime time.Time) string {
	// Last day of the month → monthly.
	nextMonth := time.Date(recordTime.Year(), recordTime.Month()+1, 1, 0, 0, 0, 0, recordTime.Location())
	if recordTime.Day() == nextMonth.AddDate(0, 0, -1).Day() {
		return types.BackupRetentionMonthly
	}
	// Sunday ends the ISO week → weekly.
	if recordTime.Weekday() == time.Sunday {
		return types.BackupRetentionWeekly
	}
	return types.BackupRetentionDaily
}

// gfsKeep returns the retention deadline per tier (a snapshot expires
// when its FinishedAt is older than the tier's horizon AND no longer
// tier requires it). tier "monthly" horizon: N3 months; "weekly": N2
// weeks; "daily": N1 days.
func gfsKeep(tag string, daily, weekly, monthly int) time.Duration {
	switch tag {
	case types.BackupRetentionMonthly:
		return time.Duration(monthly) * 30 * 24 * time.Hour
	case types.BackupRetentionWeekly:
		return time.Duration(weekly) * 7 * 24 * time.Hour
	default:
		return time.Duration(daily) * 24 * time.Hour
	}
}

// SelectExpired applies the GFS policy to succeeded snapshots and
// returns the records that should be pruned (oldest first). Snapshots
// newer than their tier horizon survive; running/failed records are
// never pruned by policy.
func SelectExpired(records []*types.BackupRecord, daily, weekly, monthly int) []*types.BackupRecord {
	now := time.Now()
	var expired []*types.BackupRecord
	for _, rec := range records {
		if rec == nil || rec.Status != types.BackupStatusSucceeded {
			continue
		}
		finished := rec.FinishedAt
		if finished == nil {
			finished = &rec.StartedAt
		}
		tag := rec.RetentionTag
		if tag == "" {
			tag = types.BackupRetentionDaily
		}
		horizon := gfsKeep(tag, daily, weekly, monthly)
		if horizon <= 0 {
			continue // tier disabled → never prune via this tier
		}
		if now.Sub(*finished) > horizon {
			expired = append(expired, rec)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		return expired[i].StartedAt.Before(expired[j].StartedAt)
	})
	return expired
}
