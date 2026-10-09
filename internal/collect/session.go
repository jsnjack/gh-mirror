package collect

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"gh-mirror/internal/config"
)

func sessionSignature(c config.Config, repos []string, generation string, full bool, token string) (string, error) {
	data, err := json.Marshal(struct {
		Version                        int
		Repositories                   []string
		Generation                     string
		API, GraphQL                   string
		Fields, Projects, Full         bool
		Overlap, Enrichment, Reconcile string
	}{1, repos, generation, c.APIURL, c.GraphQLURL, c.Fields, c.Projects, full, c.Overlap, c.EnrichmentInterval, c.ReconcileInterval})
	if err != nil {
		return "", fmt.Errorf("encode sync session: %w", err)
	}
	// Credentials affect visible scope; persist only the opaque combined identity.
	return fmt.Sprintf("%x", sha256.Sum256(append(append(data, 0), []byte(token)...))), nil
}
