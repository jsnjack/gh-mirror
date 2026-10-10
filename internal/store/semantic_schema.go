package store

const semanticSchema = `
CREATE TABLE IF NOT EXISTS embedding_cache (
 key BLOB PRIMARY KEY CHECK(length(key)=32),
 vector BLOB NOT NULL CHECK(length(vector)=1536)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS semantic_documents (
 document_id TEXT PRIMARY KEY REFERENCES documents(id) ON DELETE CASCADE,
 model TEXT NOT NULL, content_hash TEXT NOT NULL, complete INTEGER NOT NULL DEFAULT 0,
 chunks INTEGER NOT NULL DEFAULT 0, indexed_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS semantic_vectors (
 document_id TEXT NOT NULL REFERENCES semantic_documents(document_id) ON DELETE CASCADE,
 chunk INTEGER NOT NULL, field TEXT NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL,
 text TEXT NOT NULL, vector BLOB CHECK(vector IS NULL OR length(vector)=1536),
 cache_key BLOB REFERENCES embedding_cache(key),
 CHECK(vector IS NOT NULL OR cache_key IS NOT NULL),
 PRIMARY KEY(document_id,chunk)
);
CREATE INDEX IF NOT EXISTS semantic_vectors_cache ON semantic_vectors(cache_key);
DROP TRIGGER IF EXISTS semantic_invalidate;
CREATE TRIGGER semantic_invalidate AFTER UPDATE OF title,body ON documents
 WHEN old.title<>new.title OR old.body<>new.body BEGIN
 UPDATE semantic_documents SET complete=0,content_hash='' WHERE document_id=new.id;
 UPDATE metadata SET value=lower(hex(randomblob(16))) WHERE key='semantic_generation';
END;
CREATE TRIGGER IF NOT EXISTS semantic_delete AFTER DELETE ON documents BEGIN
 UPDATE metadata SET value=lower(hex(randomblob(16))) WHERE key='semantic_generation';
END;
CREATE TRIGGER IF NOT EXISTS semantic_insert AFTER INSERT ON documents BEGIN
 UPDATE metadata SET value=lower(hex(randomblob(16))) WHERE key='semantic_generation';
END;
`
