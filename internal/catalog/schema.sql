-- 来源台账。正文只保存清洗后的 UTF-8。
-- 不保存 PDF 或 DOCX 字节，不保存向量，也不保存对象键。
-- doc_id 与 parent_id 插入时由 AUTO_INCREMENT 生成。
-- chunk_id 由写入代码按文档编号、井号和序号拼出，不在表里自动生成。
-- collection_id 最长 128 个字符，source_uri 最长 512 个字符。
-- utf8mb4 下唯一索引约占 128*4+512*4=2560 字节，低于 InnoDB 的 3072 字节上限。
-- 按文档替换使用 idx_parent_doc，以及 uk_doc_ordinal 的 doc_id 前缀。
-- 按 parent 读取使用 parent_section 的主键，以及 idx_child_parent。
-- 这些索引已经存在，不另建同列索引。

CREATE TABLE IF NOT EXISTS source_document (
  doc_id BIGINT NOT NULL AUTO_INCREMENT,
  collection_id VARCHAR(128) NOT NULL,
  version INT NOT NULL,
  source_uri VARCHAR(512) NOT NULL,
  checksum CHAR(64) NOT NULL,
  media_type VARCHAR(128) NOT NULL,
  license VARCHAR(128) NOT NULL,
  title VARCHAR(512) NOT NULL,
  fetched_at DATETIME(6) NULL,
  PRIMARY KEY (doc_id),
  UNIQUE KEY uk_collection_source (collection_id, source_uri)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS parent_section (
  parent_id BIGINT NOT NULL AUTO_INCREMENT,
  doc_id BIGINT NOT NULL,
  heading_path TEXT NOT NULL,
  page_from INT NOT NULL,
  page_to INT NOT NULL,
  body MEDIUMTEXT NOT NULL,
  PRIMARY KEY (parent_id),
  KEY idx_parent_doc (doc_id),
  CONSTRAINT fk_parent_doc FOREIGN KEY (doc_id) REFERENCES source_document (doc_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS child_chunk (
  chunk_id VARCHAR(64) NOT NULL,
  parent_id BIGINT NOT NULL,
  doc_id BIGINT NOT NULL,
  ordinal INT NOT NULL,
  start_byte INT NOT NULL,
  end_byte INT NOT NULL,
  heading_path TEXT NOT NULL,
  page INT NOT NULL,
  body MEDIUMTEXT NOT NULL,
  PRIMARY KEY (chunk_id),
  UNIQUE KEY uk_doc_ordinal (doc_id, ordinal),
  KEY idx_child_parent (parent_id),
  CONSTRAINT fk_child_parent FOREIGN KEY (parent_id) REFERENCES parent_section (parent_id),
  CONSTRAINT fk_child_doc FOREIGN KEY (doc_id) REFERENCES source_document (doc_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
