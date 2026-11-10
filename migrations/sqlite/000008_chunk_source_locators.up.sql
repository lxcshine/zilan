-- FR-05 引用溯源定位：chunk 记录其内容在原始文件中的位置（页码/bbox、
-- 表格行区间、音频时间区间等），供引用接口回跳原文。
-- 对应 Postgres 000085_chunk_source_locators；空值存 NULL（无定位信息）。
ALTER TABLE chunks ADD COLUMN source_locators TEXT;
