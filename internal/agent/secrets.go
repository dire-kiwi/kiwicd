package agent

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dire-kiwi/kiwicd/internal/protocol"
)

// unseal gets a job's sealed secrets from the agent's secrets service, which
// asks the server for them with the job's token (see protocol.UnsealRequest).
func (a *Agent) unseal(ctx context.Context, req *protocol.JobRequest) (map[string]string, error) {
	if a.cfg.SecretsURL == "" {
		return nil, fmt.Errorf("this runner has no secrets service (kiwicd-agent run --secrets-url)")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c := &client{base: strings.TrimRight(a.cfg.SecretsURL, "/"), http: &http.Client{CheckRedirect: sameHost}, bearer: a.cfg.SecretsToken}
	var out protocol.UnsealResponse
	if err := c.do(ctx, "POST", "/v1/unseal", protocol.UnsealRequest{JobToken: req.Secrets[jobTokenSecret]}, &out); err != nil {
		return nil, fmt.Errorf("secrets service: %w", err)
	}
	var missing []string
	for _, name := range req.SealedSecrets {
		if _, ok := out.Secrets[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("the secrets service did not return %s", strings.Join(missing, ", "))
	}
	return out.Secrets, nil
}

const jobTokenSecret = "KIWICD_TOKEN"
