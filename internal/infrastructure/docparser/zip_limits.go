package docparser

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/utils"
)

// 解析结果包（zip）来自网络上的第三方解析 API，在校验它之前就必须先把
// 它整个缓冲进内存，因此下载量与解压后的膨胀量都需要上界（FR-07）。
//
// 所有上界都有安全默认值，且可通过环境变量调整；错误信息一律说明
// "哪一项超限、当前值与上界各是多少"，便于运维定位。
const (
	// defaultMaxResultPackageBytes 单个下载结果包的上界默认值：1 GiB。
	defaultMaxResultPackageBytes int64 = 1 << 30

	// defaultMaxZipEntryBytes 单个解压条目的上界默认值：64 MiB。
	// 一页 markdown 或一张抽取出的图片远低于此值；能触到它的条目
	// 是压缩炸弹而不是文档内容。
	defaultMaxZipEntryBytes int64 = 64 << 20

	// defaultMaxZipImagesTotalBytes 单个结果包内图片总量的预算默认值：512 MiB。
	// 每张图片都会同时驻留内存，真正决定内存占用的是总量——只约束单条目
	// 无法约束一个装着上千张小图的包。
	defaultMaxZipImagesTotalBytes int64 = 512 << 20

	// defaultMaxZipTotalUncompressedBytes 解压后总大小上界默认值：1 GiB。
	defaultMaxZipTotalUncompressedBytes int64 = 1 << 30

	// defaultMaxZipEntries 条目数上界默认值。正常结果包的条目数在数十到数百量级。
	defaultMaxZipEntries = 4096

	// defaultMaxZipCompressionRatio 压缩比（解压后/压缩前）上界默认值。
	// 正常文本条目在 20 以内，图片本身已压缩、比值接近 1；
	// 上千的比值是 zip bomb 的典型特征。
	defaultMaxZipCompressionRatio = 200

	// zipRatioMinCompressedBytes 参与压缩比判定的最小压缩后字节数。
	// 极小的条目比值天然偏高（几十十字节的头部也能"压缩"出几 KB），
	// 低于此门槛不做比值判定，避免误杀。
	zipRatioMinCompressedBytes = 4096

	// maxZipNestingDepth 允许的嵌套层数：本实现只解压一层，不递归进入
	// 包内的归档文件，因此结果包里出现归档条目即视为超出嵌套上界。
	maxZipNestingDepth = 1
)

// 上界对应的环境变量（单位 MiB，图片预算沿用上游的字节单位）。
const (
	resultPackageLimitEnv   = "WEKNORA_MAX_RESULT_PACKAGE_MB"
	zipEntryLimitEnv        = "WEKNORA_MAX_ZIP_ENTRY_MB"
	zipImagesTotalLimitEnv  = "WEKNORA_MINERU_MAX_IMAGE_BYTES"
	zipTotalUncompressedEnv = "WEKNORA_MAX_ZIP_UNCOMPRESSED_MB"
	zipEntriesLimitEnv      = "WEKNORA_MAX_ZIP_ENTRIES"
	zipCompressionRatioEnv  = "WEKNORA_MAX_ZIP_COMPRESSION_RATIO"
)

// archiveExtensions 是嵌套归档的扩展名，用于嵌套层数检查。
var archiveExtensions = []string{".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz", ".tgz"}

// errResultPackageTooLarge 标记结果包超出上述任一上界。
// 超限一律拒绝，不降级放行（PRD §5.3 第 2 条）。
var errResultPackageTooLarge = errors.New("result package exceeds maximum size")

// maxResultPackageBytes 返回结果包下载上界，可用 WEKNORA_MAX_RESULT_PACKAGE_MB 覆盖。
func maxResultPackageBytes() int64 {
	return utils.EnvMBLimit(resultPackageLimitEnv, defaultMaxResultPackageBytes)
}

// maxZipEntryBytes 返回单条目解压上界，可用 WEKNORA_MAX_ZIP_ENTRY_MB 覆盖。
func maxZipEntryBytes() int64 {
	return utils.EnvMBLimit(zipEntryLimitEnv, defaultMaxZipEntryBytes)
}

// maxZipImagesTotalBytes 返回单包图片总量预算，可用 WEKNORA_MINERU_MAX_IMAGE_BYTES 覆盖。
func maxZipImagesTotalBytes() int64 {
	return utils.EnvByteLimit(zipImagesTotalLimitEnv, defaultMaxZipImagesTotalBytes)
}

// readResultPackage 在上界内读取下载到的结果包，超限即拒绝而不是先缓冲。
// limit 非正时回落 maxResultPackageBytes()。
//
// 与 readCapped 同构：多读 1 字节，使"刚好等于上限"与"超限"可以区分。
func readResultPackage(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = maxResultPackageBytes()
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: downloaded package exceeds limit %d bytes (read %d bytes, %s)",
			errResultPackageTooLarge, limit, len(data), resultPackageLimitEnv)
	}
	return data, nil
}

// readZipEntryCapped 在上界内读取一个归档条目。
//
// 先看 UncompressedSize64，明显超限的条目连解压都不做；随后仍然用
// LimitReader 兜底读取：声明大小由厂商（或攻击者）控制，允许低报，
// 只信声明值等于没有约束。
func readZipEntryCapped(f *zip.File, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = maxZipEntryBytes()
	}
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%w: entry %q declares %d bytes, limit is %d bytes (%s)",
			errResultPackageTooLarge, f.Name, f.UncompressedSize64, limit, zipEntryLimitEnv)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: entry %q expands past %d bytes (declared %d bytes, %s)",
			errResultPackageTooLarge, f.Name, limit, f.UncompressedSize64, zipEntryLimitEnv)
	}
	return data, nil
}

// checkZipArchiveLimits 在解压任何条目之前检查整包的结构上界：
// 条目数、解压后总大小、单条目压缩比、嵌套归档。
// 声明值可被伪造，因此这里只是廉价的预筛，实际读取仍受
// readZipEntryCapped 的 LimitReader 兜底。
func checkZipArchiveLimits(zr *zip.Reader) error {
	maxEntries := int64(utils.EnvByteLimit(zipEntriesLimitEnv, defaultMaxZipEntries))
	if n := int64(len(zr.File)); n > maxEntries {
		return fmt.Errorf("%w: package has %d entries, limit is %d entries (%s)",
			errResultPackageTooLarge, n, maxEntries, zipEntriesLimitEnv)
	}

	maxTotal := utils.EnvMBLimit(zipTotalUncompressedEnv, defaultMaxZipTotalUncompressedBytes)
	maxRatio := int64(utils.EnvByteLimit(zipCompressionRatioEnv, defaultMaxZipCompressionRatio))

	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > uint64(maxTotal) {
			return fmt.Errorf("%w: uncompressed total exceeds limit %d bytes (declared %d bytes at entry %q, %s)",
				errResultPackageTooLarge, maxTotal, total, f.Name, zipTotalUncompressedEnv)
		}
		// 嵌套层数：本实现不递归解压，包内出现归档条目即超出上界。
		if isNestedArchive(f.Name) {
			return fmt.Errorf("%w: entry %q is a nested archive, nesting depth limit is %d",
				errResultPackageTooLarge, f.Name, maxZipNestingDepth)
		}
		if f.CompressedSize64 >= zipRatioMinCompressedBytes &&
			f.UncompressedSize64 > uint64(maxRatio)*f.CompressedSize64 {
			return fmt.Errorf("%w: entry %q compression ratio %d exceeds limit %d (%d -> %d bytes, %s)",
				errResultPackageTooLarge, f.Name, f.UncompressedSize64/f.CompressedSize64, maxRatio,
				f.CompressedSize64, f.UncompressedSize64, zipCompressionRatioEnv)
		}
	}
	return nil
}

// isNestedArchive 依据扩展名判断条目是否本身就是一个归档文件。
func isNestedArchive(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, archive := range archiveExtensions {
		if ext == archive {
			return true
		}
	}
	return false
}
