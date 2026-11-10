-- FR-18 文档图片画廊：回滚 chunk_images 投影表（PostgreSQL 方言）。
-- 从上游 WeKnora-main migrations/versioned/000113_chunk_images.down.sql 照抄。
DROP TRIGGER IF EXISTS trg_chunk_images_insert ON chunks;
DROP TRIGGER IF EXISTS trg_chunk_images_update ON chunks;
DROP TRIGGER IF EXISTS trg_chunk_images_delete ON chunks;
DROP FUNCTION IF EXISTS chunk_images_sync();
DROP FUNCTION IF EXISTS chunk_images_try_jsonb(TEXT);
DROP TABLE IF EXISTS chunk_images;
