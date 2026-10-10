package store

import (
	"context"
	"database/sql"
	"fmt"
)

func migrateVectorStorage(tx *sql.Tx) error {
	var exists, updated int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='semantic_vectors'").Scan(&exists); err != nil {
		return fmt.Errorf("inspect vector storage: %w", err)
	}
	if exists == 0 {
		return nil
	}
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('semantic_vectors') WHERE name='cache_key'").Scan(&updated); err != nil {
		return fmt.Errorf("inspect shared vector storage: %w", err)
	}
	if updated > 0 {
		return nil
	}
	if _, err := tx.Exec("ALTER TABLE semantic_vectors RENAME TO semantic_vectors_legacy;" + semanticSchema + `INSERT INTO semantic_vectors(document_id,chunk,field,start,end,text,vector) SELECT document_id,chunk,field,start,end,text,vector FROM semantic_vectors_legacy; DROP TABLE semantic_vectors_legacy;`); err != nil {
		return fmt.Errorf("migrate vector storage locally: %w", err)
	}
	return nil
}

func vectorStorage(ctx context.Context, q querier) (string, string, error) {
	var version string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='schema_version'").Scan(&version); err != nil {
		return "", "", fmt.Errorf("read vector storage version: %w", err)
	}
	if version == "3" {
		return "COALESCE(v.vector,cache.vector)", " LEFT JOIN embedding_cache cache ON cache.key=v.cache_key ", nil
	}
	return "v.vector", " ", nil
}
