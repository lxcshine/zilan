package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gorm.io/gorm"
)

// tableSpec describes one discoverable table for the metadata tier.
//
// Tables are DISCOVERED dynamically from the schema (not a hardcoded
// whitelist): a new table lands in the next snapshot automatically,
// which structurally satisfies the PRD's "no table silently missed"
// requirement (data-backup-recovery.md §4.2, DoD §11.7).
type tableSpec struct {
	Name string
	// TenantScoped: the table has a tenant_id column and participates
	// in per-workspace export/restore.
	TenantScoped bool
	// Order is the restore insertion rank (PRD §4.2 "外键依赖拓扑排序").
	// Known-core tables carry curated ranks; unknown tables sort last.
	Order int
	// HasDeletedAt: gorm soft-delete column present.
	HasDeletedAt bool
}

// restoreOrder is the curated topological order for the core business
// tables. Anything not listed here gets order 900 (restored after all
// known tables) — a safe default because unknown tables are assumed to
// depend on (reference) core entities, never the reverse.
var restoreOrder = map[string]int{
	"tenants":                    10,
	"users":                      20,
	"tenant_members":             30,
	"tenant_invitations":         40,
	"tenant_api_keys":            50,
	"models":                     60,
	"vector_stores":              70,
	"storage_backends":           80,
	"mcp_services":               90,
	"web_search_providers":       100,
	"knowledge_bases":            200,
	"knowledge_tags":             210,
	"knowledge":                  220,
	"knowledge_chunks":           230,
	"agents":                     300,
	"agent_kb_bindings":          310,
	"sessions":                   400,
	"messages":                   410,
	"memory_facts":               500,
	"memory_summaries":           510,
	"tenant_settings":            600,
	"user_resource_favorites":    610,
	"audit_logs":                 700,
	"verification_codes":         710,
	"organizations":              800,
	"organization_members":       810,
	"kb_shares":                  820,
	"agent_shares":               830,
	"shared_knowledge_bases":     840,
	"tenant_disabled_shared_agents": 850,
}

// discoverTables introspects the schema for all user tables and whether
// each carries tenant_id / deleted_at columns. Works on PostgreSQL
// (information_schema) and SQLite (pragma) alike.
func discoverTables(ctx context.Context, db *gorm.DB) ([]*tableSpec, error) {
	var rows []map[string]any
	var err error

	dialect := db.Dialector.Name()
	switch dialect {
	case "sqlite":
		rows, err = scanRows(db, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			return nil, fmt.Errorf("list sqlite tables: %w", err)
		}
	default:
		rows, err = scanRows(db, `
			SELECT table_name AS name
			FROM information_schema.tables
			WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
			  AND table_name NOT IN ('schema_migrations', 'goose_db_version')
		`)
		if err != nil {
			return nil, fmt.Errorf("list postgres tables: %w", err)
		}
	}

	specs := make([]*tableSpec, 0, len(rows))
	for _, row := range rows {
		raw, _ := row["name"].(string)
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		spec := &tableSpec{Name: name, Order: 900}

		// Column introspection is best-effort per dialect.
		switch dialect {
		case "sqlite":
			cols, colErr := scanRows(db, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(dialect, name)))
			if colErr == nil {
				for _, c := range cols {
					if cn, _ := c["name"].(string); cn == "tenant_id" {
						spec.TenantScoped = true
					}
					if cn, _ := c["name"].(string); cn == "deleted_at" {
						spec.HasDeletedAt = true
					}
				}
			}
		default:
			cols, colErr := scanRows(db, `
				SELECT column_name AS name
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = `+quoteLiteral(name))
			if colErr == nil {
				for _, c := range cols {
					if cn, _ := c["name"].(string); cn == "tenant_id" {
						spec.TenantScoped = true
					}
					if cn, _ := c["name"].(string); cn == "deleted_at" {
						spec.HasDeletedAt = true
					}
				}
			}
		}
		if rank, ok := restoreOrder[name]; ok {
			spec.Order = rank
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func quoteIdent(dialect, ident string) string {
	if dialect == "sqlite" {
		return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
	}
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func quoteLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// scanRows runs a raw SELECT and returns rows as ordered maps.
func scanRows(db *gorm.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Raw(query, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []map[string]any
	cols, _ := rows.Columns()
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			v := values[i]
			// Normalize []byte to string for stable JSON encoding.
			if b, ok := v.([]byte); ok {
				row[strings.ToLower(c)] = string(b)
			} else {
				row[strings.ToLower(c)] = v
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// jsonlRecord is one exported row.
type jsonlRecord struct {
	Table string         `json:"table"`
	Row   map[string]any `json:"row"`
}

// exportRows streams rows of one table (optionally tenant-filtered) to
// the jsonl writer. Soft-deleted rows are INCLUDED: the snapshot must be
// a faithful copy so a restored instance can un-delete.
func exportRows(ctx context.Context, db *gorm.DB, spec *tableSpec, tenantID uint64, w *json.Encoder) (int64, error) {
	query := fmt.Sprintf(`SELECT * FROM %s`, quoteIdent(db.Dialector.Name(), spec.Name))
	var args []any
	if spec.TenantScoped && tenantID > 0 {
		query += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	query += ` ORDER BY 1`

	rows, err := db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return 0, fmt.Errorf("select %s: %w", spec.Name, err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var count int64
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, fmt.Errorf("scan %s: %w", spec.Name, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := values[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = values[i]
			}
		}
		if err := w.Encode(jsonlRecord{Table: spec.Name, Row: row}); err != nil {
			return count, fmt.Errorf("encode %s row: %w", spec.Name, err)
		}
		count++
	}
	return count, rows.Err()
}

// importRow inserts one exported row. On primary-key conflict the row is
// skipped and reported (PRD §5.4: 跳过 ID 冲突行并记录到报告).
func importRow(ctx context.Context, db *gorm.DB, spec *tableSpec, row map[string]any) (inserted bool, err error) {
	if len(row) == 0 {
		return false, nil
	}
	cols := make([]string, 0, len(row))
	placeholders := make([]string, 0, len(row))
	args := make([]any, 0, len(row))
	for c, v := range row {
		cols = append(cols, quoteIdent(db.Dialector.Name(), c))
		placeholders = append(placeholders, "?")
		args = append(args, v)
	}
	query := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING`,
		quoteIdent(db.Dialector.Name(), spec.Name),
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
	)
	res := db.WithContext(ctx).Exec(query, args...)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// jsonlScanner iterates a jsonl stream.
type jsonlScanner struct {
	scanner *bufio.Scanner
	decoder *json.Decoder
}

func newJSONLScanner(r io.Reader) *jsonlScanner {
	return &jsonlScanner{decoder: json.NewDecoder(r)}
}

func (s *jsonlScanner) next() (*jsonlRecord, error) {
	var rec jsonlRecord
	if err := s.decoder.Decode(&rec); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// specIndex maps table name → spec for import-side lookups.
func specIndex(specs []*tableSpec) map[string]*tableSpec {
	m := make(map[string]*tableSpec, len(specs))
	for _, s := range specs {
		m[s.Name] = s
	}
	return m
}

// sortedForRestore orders specs so referenced tables insert before
// referencing ones (restoreOrder rank, then name).
func sortedForRestore(specs []*tableSpec) []*tableSpec {
	out := append([]*tableSpec(nil), specs...)
	// Simple stable insertion by (Order, Name) — table counts are small
	// (a few dozen), no need for sort.Slice machinery beyond this.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Order < out[j-1].Order ||
			(out[j].Order == out[j-1].Order && out[j].Name < out[j-1].Name)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
