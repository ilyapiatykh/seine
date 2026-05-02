package controlplane

import (
	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/spec"
)

// SpecProvider is the read interface the server needs against the network
// specification. The concrete implementation in cmd/seine-server is a
// Git-pulling poller; tests use an in-memory provider.
type SpecProvider interface {
	// Current returns the most recent successfully parsed spec along with
	// its commit SHA. Implementations must be safe for concurrent use.
	Current() (doc *spec.Document, commitSHA string, err error)
}

// resolveRole inspects the spec to classify a name as a hub or spoke. An
// "unspecified" return means the name is not declared at all.
func resolveRole(d *spec.Document, name string) (cpv1.Role, *spec.Hub, *spec.Agent) {
	if d == nil {
		return cpv1.Role_ROLE_UNSPECIFIED, nil, nil
	}
	if h := d.FindHub(name); h != nil {
		return cpv1.Role_ROLE_HUB, h, nil
	}
	if a := d.FindAgent(name); a != nil {
		return cpv1.Role_ROLE_SPOKE, nil, a
	}
	return cpv1.Role_ROLE_UNSPECIFIED, nil, nil
}

// tunnelIPFor returns the spec-declared overlay address for a name, or "".
func tunnelIPFor(d *spec.Document, name string) string {
	if d == nil {
		return ""
	}
	if h := d.FindHub(name); h != nil {
		return h.TunnelIP
	}
	if a := d.FindAgent(name); a != nil {
		return a.TunnelIP
	}
	return ""
}
