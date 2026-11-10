-- FR-18 文档图片画廊：回滚 chunk_images 投影表（SQLite 方言）。
-- 从上游 WeKnora-main migrations/sqlite/000032_chunk_images.down.sql 照抄。
DROP TRIGGER IF EXISTS trg_chunk_images_insert;
DROP TRIGGER IF EXISTS trg_chunk_images_patch;
DROP TRIGGER IF EXISTS trg_chunk_images_update;
DROP TRIGGER IF EXISTS trg_chunk_images_delete;
DROP TABLE IF EXISTS chunk_images;
