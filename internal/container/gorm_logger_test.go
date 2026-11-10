package container

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// FR-08 条 2：慢查询日志不得包含绑定值（chunk 正文、向量、api_key/token 列）。
// ParameterizedQueries 让 GORM 在打印前就把参数丢掉，只留 SQL 与耗时。
func TestNewGormLogger_ParameterizedQueries(t *testing.T) {
	var buf bytes.Buffer
	l := newGormLogger(&buf)

	filter, ok := l.(gorm.ParamsFilter)
	if !ok {
		t.Fatal("logger does not implement gorm.ParamsFilter; bound values would be printed")
	}
	const secret = "sk-live-super-secret"
	sql, vars := filter.ParamsFilter(context.Background(),
		"SELECT * FROM tenants WHERE api_key = ?", secret)
	if len(vars) != 0 {
		t.Errorf("ParamsFilter kept %d bound values, want 0", len(vars))
	}
	if !strings.Contains(sql, "?") {
		t.Errorf("sql = %q, want the placeholder preserved", sql)
	}
	if strings.Contains(sql, secret) {
		t.Errorf("sql = %q, want no bound value interpolated", sql)
	}

	// 慢查询日志走同一条路径：只应看到占位符。
	l.Trace(context.Background(), time.Now().Add(-2*gormSlowSQLThreshold),
		func() (string, int64) { return sql, 1 }, nil)
	out := buf.String()
	if !strings.Contains(out, "SLOW SQL") {
		t.Fatalf("slow query not logged: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("bound value leaked into slow-query log: %q", out)
	}
	if !strings.Contains(out, "api_key = ?") {
		t.Errorf("parameterized SQL missing from log: %q", out)
	}
}

// 快于阈值的语句不该打印，避免把正常查询灌进 stdout。
func TestNewGormLogger_FastQueryNotLogged(t *testing.T) {
	var buf bytes.Buffer
	l := newGormLogger(&buf)
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		return "SELECT 1", 1
	}, nil)
	if buf.Len() != 0 {
		t.Errorf("fast query was logged: %q", buf.String())
	}
}

// 慢查询阈值必须是 GORM 的默认值 200ms。
func TestGormSlowSQLThreshold(t *testing.T) {
	if gormSlowSQLThreshold != 200*time.Millisecond {
		t.Errorf("gormSlowSQLThreshold = %v, want 200ms", gormSlowSQLThreshold)
	}
}
