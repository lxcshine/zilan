package container

import (
	"io"
	stdlog "log"
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// gormSlowSQLThreshold 是 GORM 默认的慢查询阈值，低于该耗时的语句不会以
// Warn 级别打印。
const gormSlowSQLThreshold = 200 * time.Millisecond

// newGormLogger 构造应用使用的数据库日志器（FR-08）。
//
// 关键点是 ParameterizedQueries: true —— 慢查询日志保留 SQL 文本与耗时，
// 但丢弃绑定值。默认的 gormlogger.Default 会把参数原值打出来，而本项目的
// 绑定值里包含 chunk 正文、向量数组，以及 api_key / token 之类的凭据列，
// 一旦落到 stdout 就等于把凭据写进日志。
//
// 其余配置与 GORM 默认保持一致：Warn 级别、200ms 慢查询阈值、
// 不忽略 RecordNotFound（业务侧依赖它排查问题）。
func newGormLogger(w io.Writer) gormlogger.Interface {
	return gormlogger.New(stdlog.New(w, "\r\n", stdlog.LstdFlags), gormlogger.Config{
		SlowThreshold:             gormSlowSQLThreshold,
		LogLevel:                  gormlogger.Warn,
		IgnoreRecordNotFoundError: false,
		Colorful:                  true,
		ParameterizedQueries:      true,
	})
}
