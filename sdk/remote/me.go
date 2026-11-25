package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Account is the public identity returned by GET /v1/me.
type Account struct {
	ID        string          `json:"id"`
	Email     string          `json:"email"`
	CreatedAt time.Time       `json:"createdAt"`
	Profile   json.RawMessage `json:"profile,omitempty"`
}

// Workspace is a membership entry in the caller's account identity.
type Workspace struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Revision       int64     `json:"revision"`
	Role           string    `json:"role,omitempty"`
	AgentAuthority Authority `json:"agentAuthority"`
}

// AccountIdentity is the GET /v1/me response: the caller's account plus its
// current workspace memberships. It never contains credentials.
type AccountIdentity struct {
	Account            Account     `json:"account"`
	Workspaces         []Workspace `json:"workspaces"`
	OnboardingComplete bool        `json:"onboardingComplete"`
}

// Me inspects the account behind a personal API key (canter_pk_...), for
// example one exported as CANTER_API_KEY. Agent installation access tokens
// are not accepted by this endpoint; use WhoAmI for those. The key is sent
// only to the configured Canter origin and is never decoded or stored.
func (c *Client) Me(ctx context.Context, apiKey string) (AccountIdentity, error) {
	var result AccountIdentity
	err := c.do(ctx, http.MethodGet, "/v1/me", apiKey, nil, &result)
	return result, err
}
