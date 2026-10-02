// Package secrets provides SecretResolver implementations for macro replay.
//
// The only implementation is the 1Password service-account bridge: it turns
// op:// references (op://vault/item/field) into values at replay time inside
// the gateway process. Values exist only inside the replay call frame; they
// never enter macro files, evidence logs, or the agent-facing API.
//
// The resolver is wired only when BROWSER_FETCH_OP_TOKEN is set in the
// gateway's environment (mounted from a Kubernetes Secret by the operator).
// Without the token the gateway runs resolver-less and type steps abort —
// the same fail-closed behavior as before this package existed.
package secrets

import (
	"context"
	"fmt"
	"strings"

	"github.com/1password/onepassword-sdk-go"
)

// integrationName/Version identify this integration to the 1Password API.
const (
	integrationName    = "browser-fetch"
	integrationVersion = "v0.1.0"
)

// OnePassword implements macro.SecretResolver backed by a 1Password service
// account token. It resolves the exact reference syntax 1Password documents:
// op://<vault>/<item>[/<section>]/<field>.
type OnePassword struct {
	client *onepassword.Client
}

// NewOnePassword builds a resolver around a service account token. The token
// is held only in memory and sent only to 1Password's API by the SDK.
func NewOnePassword(ctx context.Context, serviceAccountToken string) (*OnePassword, error) {
	client, err := onepassword.NewClient(ctx,
		onepassword.WithServiceAccountToken(serviceAccountToken),
		onepassword.WithIntegrationInfo(integrationName, integrationVersion),
	)
	if err != nil {
		return nil, fmt.Errorf("1password client: %w", err)
	}
	return &OnePassword{client: client}, nil
}

// Resolve implements macro.SecretResolver. The context bounds the call; the
// resolved value must stay within the replay call frame.
func (o *OnePassword) Resolve(ctx context.Context, ref string) (string, error) {
	if o == nil || o.client == nil {
		return "", fmt.Errorf("1password resolver not configured")
	}
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "op://") {
		return "", fmt.Errorf("not an op:// reference")
	}
	value, err := o.client.Secrets().Resolve(ctx, ref)
	if err != nil {
		// Error text may echo the reference (fine — it contains no value)
		// but must never echo the value itself; the SDK's errors do not.
		return "", fmt.Errorf("op resolve: %w", err)
	}
	return value, nil
}
