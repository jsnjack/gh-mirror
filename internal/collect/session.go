package collect

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"gh-mirror/internal/config"
)

const checkpointVersion = 1

func credentialIdentity(c config.Config, token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(c.APIURL+"\x00"+c.GraphQLURL+"\x00"+token)))
}

func sessionSignature(c config.Config, repos []string, generation string, full bool, token string, version int) (string, error) {
	options := map[string]config.Scope{}
	for _, repo := range repos {
		if scope := c.Scope(repo); scope != c.DefaultScope() {
			options[repo] = scope
		}
	}
	data, err := json.Marshal(struct {
		Version                        int
		CollectionVersion              int                     `json:",omitempty"`
		RepositoryOptions              map[string]config.Scope `json:",omitempty"`
		Repositories                   []string
		Generation                     string
		API, GraphQL                   string
		Fields, Projects, Full         bool
		Overlap, Enrichment, Reconcile string
	}{checkpointVersion, version, options, repos, generation, c.APIURL, c.GraphQLURL, c.Fields, c.Projects, full, c.Overlap, c.EnrichmentInterval, c.ReconcileInterval})
	if err != nil {
		return "", fmt.Errorf("encode sync session: %w", err)
	}
	// Credentials affect visible scope; persist only the opaque combined identity.
	return fmt.Sprintf("%x", sha256.Sum256(append(append(data, 0), []byte(token)...))), nil
}
