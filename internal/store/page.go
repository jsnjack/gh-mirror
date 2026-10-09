package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// ErrInvalidQuery identifies invalid local query input.
var ErrInvalidQuery = errors.New("invalid query")

// ErrCursorConflict identifies a cursor from a different committed generation.
var ErrCursorConflict = errors.New("cursor generation conflict")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidQuery, fmt.Sprintf(format, args...))
}
func hash(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

type pageCursor struct {
	Generation string  `json:"generation"`
	Signature  string  `json:"signature"`
	Kind       string  `json:"kind"`
	Repo       string  `json:"repo,omitempty"`
	Number     int     `json:"number,omitempty"`
	Key        string  `json:"key,omitempty"`
	Score      float64 `json:"score,omitempty"`
	ID         string  `json:"id,omitempty"`
}

func decodeCursor(raw, kind, sig, generation string) (pageCursor, error) {
	var c pageCursor
	if raw == "" {
		return c, nil
	}
	if len(raw) > 4096 {
		return c, invalid("cursor exceeds 4096 bytes")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, invalid("invalid cursor encoding")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, invalid("invalid cursor data")
	}
	if c.Kind != kind || c.Signature != sig || math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
		return c, invalid("cursor does not match query or filters")
	}
	if c.Generation != generation {
		return c, fmt.Errorf("%w; restart enumeration", ErrCursorConflict)
	}
	return c, nil
}
func encodeCursor(c pageCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode continuation: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return 30, nil
	}
	if limit < 1 || limit > 100 {
		return 0, invalid("limit must be between 1 and 100")
	}
	return limit, nil
}

func orderSQL(p PageOptions) string {
	key := "repo"
	switch p.Sort {
	case "relevance":
		key = "score"
	case "updated", "created":
		key = p.Sort + "_at"
	}
	if key == "repo" {
		return "repo " + p.Order + ",number " + p.Order
	}
	return key + " " + p.Order + ",repo ASC,number ASC"
}
func continuation(p PageOptions, c pageCursor) (string, []any) {
	if c.Repo == "" {
		return "1=1", nil
	}
	op := ">"
	if p.Order == "desc" {
		op = "<"
	}
	if p.Sort == "number" {
		return "(repo" + op + "? OR (repo=? AND number" + op + "?))", []any{c.Repo, c.Repo, c.Number}
	}
	key := p.Sort + "_at"
	var value any = c.Key
	if p.Sort == "relevance" {
		key = "score"
		value = c.Score
	}
	return "(" + key + op + "? OR (" + key + "=? AND (repo>? OR (repo=? AND number>?))))", []any{value, value, c.Repo, c.Repo, c.Number}
}
