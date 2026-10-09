package store

const schema = `
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS issues (
 repo TEXT NOT NULL, number INTEGER NOT NULL, node_id TEXT NOT NULL, kind TEXT NOT NULL,
 title TEXT NOT NULL, body TEXT NOT NULL, state TEXT NOT NULL, updated_at TEXT NOT NULL,
 url TEXT NOT NULL, payload TEXT NOT NULL CHECK(json_valid(payload)),
 fields TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(fields)),
 extra TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(extra)),
 PRIMARY KEY(repo,number), UNIQUE(node_id)
);
CREATE TABLE IF NOT EXISTS comments (
 id TEXT PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL,
 body TEXT NOT NULL, updated_at TEXT NOT NULL, url TEXT NOT NULL,
 payload TEXT NOT NULL CHECK(json_valid(payload)),
 FOREIGN KEY(repo,number) REFERENCES issues(repo,number) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments(repo,number);
CREATE TABLE IF NOT EXISTS catalog (
 kind TEXT NOT NULL, scope TEXT NOT NULL, id TEXT NOT NULL, payload TEXT NOT NULL CHECK(json_valid(payload)),
 PRIMARY KEY(kind,scope,id)
);
CREATE TABLE IF NOT EXISTS responses (url TEXT PRIMARY KEY, etag TEXT NOT NULL, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS sync_status (
 repo TEXT PRIMARY KEY, collected_at TEXT NOT NULL, reconciled_at TEXT NOT NULL,
 fields TEXT NOT NULL, projects TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS documents (
 id TEXT PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL, source TEXT NOT NULL,
 title TEXT NOT NULL, body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS documents_issue ON documents(repo,number);
CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(title,body,content='documents',content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS documents_insert AFTER INSERT ON documents BEGIN
 INSERT INTO documents_fts(rowid,title,body) VALUES(new.rowid,new.title,new.body);
END;
CREATE TRIGGER IF NOT EXISTS documents_delete AFTER DELETE ON documents BEGIN
 INSERT INTO documents_fts(documents_fts,rowid,title,body) VALUES('delete',old.rowid,old.title,old.body);
END;
CREATE TRIGGER IF NOT EXISTS documents_update AFTER UPDATE ON documents BEGIN
 INSERT INTO documents_fts(documents_fts,rowid,title,body) VALUES('delete',old.rowid,old.title,old.body);
 INSERT INTO documents_fts(rowid,title,body) VALUES(new.rowid,new.title,new.body);
END;
CREATE TRIGGER IF NOT EXISTS issue_delete AFTER DELETE ON issues BEGIN
 DELETE FROM documents WHERE repo=old.repo AND number=old.number;
END;
CREATE TRIGGER IF NOT EXISTS comment_delete AFTER DELETE ON comments BEGIN
 DELETE FROM documents WHERE id='comment:'||old.id;
END;
`
