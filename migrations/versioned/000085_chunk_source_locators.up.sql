-- FR-05 引用溯源定位：chunk 记录其内容在原始文件中的位置（页码/bbox、
-- 表格行区间、音频时间区间等），供引用接口回跳原文。
-- 空值存 NULL：存量文档不做自动重解析，读取侧容忍空值。
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS source_locators JSONB;
