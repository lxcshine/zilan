package docparser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// FR-14：MinerU Cloud 对单文件的页数与体积都有硬上限，超限的大 PDF 需要
// 在本地切片后逐片解析再合并。切片用 qpdf 完成（不做栅格化，保留矢量与
// 文本层），临时目录在函数返回时整体删除，分片文件读完即删，不留孤儿文件。

// cloudPDFPartPages 单分片的页数上限（MinerU Cloud 单次任务的页数配额）。
const cloudPDFPartPages = 100

// cloudPDFPartBytes 单分片的体积上限（MinerU Cloud 单文件上传上限）。
const cloudPDFPartBytes = 10 * 1024 * 1024

type pdfPart struct {
	content     []byte
	first, last int
}

// qpdfSucceeded 报告一次 qpdf 调用的产物是否可用。qpdf 的退出码 3 表示
// "输入有问题但已带警告修复成功"，文件已经正常写出，把它当错误会让
// 略有瑕疵的 PDF 完全无法解析。
func qpdfSucceeded(err error) bool {
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 3
}

// splitCloudPDF bounds both page count and upload size, without rasterizing
// PDFs. qpdf 的退出码 3 表示"带警告修复成功"，产物仍然可用，不能当错误。
func splitCloudPDF(ctx context.Context, content []byte) ([]pdfPart, error) {
	dir, err := os.MkdirTemp("", "mineru-pdf-")
	if err != nil {
		return nil, err
	}
	// 中间产物清理（FR-14 ④）：无论成功、失败还是 panic，临时目录整体删除。
	defer func() { _ = os.RemoveAll(dir) }()
	input := filepath.Join(dir, "input.pdf")
	if err = os.WriteFile(input, content, 0o600); err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "qpdf", "--show-npages", input).Output()
	if err != nil {
		if !qpdfSucceeded(err) {
			return nil, fmt.Errorf("PDF page inspection: %w", err)
		}
		logger.Warnf(ctx, "[MinerUCloud] qpdf repaired the PDF with warnings: %v", err)
	}
	pages, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pages < 1 {
		return nil, fmt.Errorf("invalid PDF page count %q", out)
	}
	if pages <= cloudPDFPartPages && len(content) <= cloudPDFPartBytes {
		return []pdfPart{{content, 1, pages}}, nil
	}
	var parts []pdfPart
	var extract func(int, int) error
	extract = func(first, last int) error {
		path := filepath.Join(dir, fmt.Sprintf("%d-%d.pdf", first, last))
		output, e := exec.CommandContext(ctx, "qpdf", "--deterministic-id", input, "--pages", ".",
			fmt.Sprintf("%d-%d", first, last), "--", path).CombinedOutput()
		if e != nil && !qpdfSucceeded(e) {
			return fmt.Errorf("split PDF pages %d-%d: %s: %w", first, last, output, e)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		// 分片已经读进内存，立即删掉磁盘副本，避免大文档在临时目录里堆积。
		_ = os.Remove(path)
		if len(b) > cloudPDFPartBytes && first < last {
			mid := (first + last) / 2
			if e = extract(first, mid); e != nil {
				return e
			}
			return extract(mid+1, last)
		}
		parts = append(parts, pdfPart{b, first, last})
		return nil
	}
	for first := 1; first <= pages; first += cloudPDFPartPages {
		last := first + cloudPDFPartPages - 1
		if last > pages {
			last = pages
		}
		if err = extract(first, last); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

// readCloudPart 解析单个文件（或单个分片），并保留布局块用于溯源。
func (c *MinerUCloudReader) readCloudPart(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	batch, uploadURL, err := c.applyUploadURLs(ctx, req.FileName, filepath.Ext(req.FileName))
	if err != nil {
		return nil, fmt.Errorf("MinerU Cloud apply upload URLs: %w", err)
	}
	if err = c.uploadFile(ctx, uploadURL, req.FileContent); err != nil {
		return nil, fmt.Errorf("MinerU Cloud file upload: %w", err)
	}
	md, refs, contentList, err := c.pollBatchResult(ctx, batch)
	if err != nil {
		return nil, fmt.Errorf("MinerU Cloud poll: %w", err)
	}
	return &types.ReadResult{
		MarkdownContent: md,
		ImageRefs:       refs,
		SourceBlocks:    minerUSourceBlocks(md, contentList, minerUFileTypeOf(req)),
	}, nil
}

// readSplitPDF 把大 PDF 切片后逐片解析并合并结果：图片引用按分片命名空间化，
// SourceBlock 的页码平移回原始 PDF 的页号，rune 偏移按合并后的位置重算。
func (c *MinerUCloudReader) readSplitPDF(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	// Standalone/desktop deployments can still parse small PDFs without qpdf.
	// Docker includes qpdf; other deployments need it for automatic splitting.
	if _, err := exec.LookPath("qpdf"); err != nil {
		logger.Warnf(ctx, "[MinerUCloud] qpdf not found in PATH; large PDFs cannot be split automatically: %v", err)
		result, readErr := c.readCloudPart(ctx, req)
		if readErr != nil && strings.Contains(strings.ToLower(readErr.Error()), "exceeds limit") {
			return nil, fmt.Errorf("install qpdf to automatically split this large PDF: %w", readErr)
		}
		return result, readErr
	}
	parts, err := splitCloudPDF(ctx, req.FileContent)
	if err != nil {
		return nil, err
	}
	logger.Infof(ctx, "[MinerUCloud] PDF split into %d parts: %s", len(parts), req.FileName)
	merged := &types.ReadResult{Metadata: map[string]string{
		"pages":     strconv.Itoa(parts[len(parts)-1].last),
		"ocr_parts": strconv.Itoa(len(parts)),
	}}
	var markdown strings.Builder
	for i, part := range parts {
		sub := *req
		sub.FileContent = part.content
		sub.FileName = fmt.Sprintf("%s-pages-%d-%d.pdf",
			strings.TrimSuffix(req.FileName, filepath.Ext(req.FileName)), part.first, part.last)
		sub.FileType = "pdf"
		result, e := c.readCloudPart(ctx, &sub)
		if e != nil {
			return nil, fmt.Errorf("OCR part %d/%d (pages %d-%d): %w", i+1, len(parts), part.first, part.last, e)
		}
		md := result.MarkdownContent
		blocks := append([]types.SourceBlock(nil), result.SourceBlocks...)
		// MinerU parts can reuse image filenames. Namespace references before merging.
		for _, ref := range result.ImageRefs {
			old := ref.OriginalRef
			ref.Filename = fmt.Sprintf("part-%d-%s", i+1, ref.Filename)
			ref.OriginalRef = fmt.Sprintf("part-%d/%s", i+1, old)
			runes := []rune(md)
			for j := range blocks {
				block := &blocks[j]
				start := utf8.RuneCountInString(replacePartImageRef(string(runes[:block.Start]), old, ref.OriginalRef))
				end := utf8.RuneCountInString(replacePartImageRef(string(runes[:block.End]), old, ref.OriginalRef))
				block.Start, block.End = start, end
			}
			md = replacePartImageRef(md, old, ref.OriginalRef)
			merged.ImageRefs = append(merged.ImageRefs, ref)
		}
		if i > 0 {
			markdown.WriteString("\n\n")
		}
		fmt.Fprintf(&markdown, "<!-- Original PDF pages %d-%d -->\n\n", part.first, part.last)
		// 合并后的偏移 = 已经写入的合并文本长度；页码平移回原始 PDF 的页号。
		offset := utf8.RuneCountInString(markdown.String())
		for _, block := range blocks {
			block.Locator.Page += part.first - 1
			block.Locator.SourceID = fmt.Sprintf("part-%d/%s", i+1, block.Locator.SourceID)
			block.Start += offset
			block.End += offset
			merged.SourceBlocks = append(merged.SourceBlocks, block)
		}
		markdown.WriteString(md)
		logger.Infof(ctx, "[MinerUCloud] Completed part %d/%d pages %d-%d", i+1, len(parts), part.first, part.last)
	}
	merged.MarkdownContent = markdown.String()
	return merged, nil
}

// replacePartImageRef 把分片内的图片引用改写为带分片命名空间的引用，
// markdown 语法与内联 HTML 的 src 属性都要覆盖。
func replacePartImageRef(markdown, old, replacement string) string {
	markdown = strings.ReplaceAll(markdown, "]("+old+")", "]("+replacement+")")
	markdown = strings.ReplaceAll(markdown, `src="`+old+`"`, `src="`+replacement+`"`)
	return strings.ReplaceAll(markdown, "src='"+old+"'", "src='"+replacement+"'")
}
