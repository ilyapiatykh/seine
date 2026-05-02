// Package spec defines the declarative network specification that operators
// commit to a Git repository and that agents consume to reconcile WireGuard
// state.
//
// The schema follows a Kubernetes-style envelope:
//
//	apiVersion: seine.io/v1
//	kind: Network
//	metadata: { name, description }
//	spec: { cidr, mtu, hubs[], agents[], groups[], acls[] }
//
// Document is the wire format (strings throughout, no parsing). After Parse()
// a Document can be relied upon as semantically valid; helper accessors do the
// minimal re-parsing that callers need.
package spec

import "time"

// API constants validated by Document.Validate.
const (
	APIVersion = "seine.io/v1"
	Kind       = "Network"

	DefaultMTU                 = 1420
	DefaultPersistentKeepalive = 25 * time.Second
	DefaultHubListenPort       = 51820
)

// Action is the verb of an ACL rule.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
)

// Document is the top-level YAML object that lives in the Git repository.
type Document struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Network  `yaml:"spec"`
}

// Metadata carries network-level human-readable identifiers.
type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// Network is the body of the spec.
type Network struct {
	// CIDR is the overlay subnet (e.g. 100.64.0.0/10 in the thesis demo).
	// Every hub and agent tunnel IP must fall inside this prefix.
	CIDR string `yaml:"cidr"`

	// WireGuard contains tunnel-level tuning. Optional; defaults applied.
	WireGuard WireGuard `yaml:"wireguard,omitempty"`

	// Hubs are well-known transit nodes with public endpoints. They run on
	// Linux (kernel WireGuard) and are connected in a full mesh.
	Hubs []Hub `yaml:"hubs"`

	// Agents are the spokes — workstations and servers that join the
	// network. Each agent is anchored to one hub.
	Agents []Agent `yaml:"agents"`

	// Groups declares the universe of group identifiers referenced by
	// Agent.Groups and ACL.From/To.
	Groups []string `yaml:"groups,omitempty"`

	// ACLs express which groups may communicate. The default policy is
	// deny: traffic between groups is forwarded only if some allow rule
	// matches and no deny rule overrides it.
	ACLs []ACL `yaml:"acls,omitempty"`
}

// WireGuard holds tunnel-level settings shared by all peers.
type WireGuard struct {
	// MTU of the wgN interface. Defaults to 1420 (per WireGuard guidance
	// for IPv4 over typical Ethernet, restated in the thesis).
	MTU int `yaml:"mtu,omitempty"`

	// PersistentKeepalive is sent by spokes toward their hub so that
	// stateful NAT mappings stay open without external NAT traversal.
	PersistentKeepalive Duration `yaml:"persistentKeepalive,omitempty"`

	// HubListenPort is the default UDP port hubs bind to. Per-hub
	// overrides live on the Hub.Endpoint host:port pair.
	HubListenPort int `yaml:"hubListenPort,omitempty"`
}

// Hub is a transit node with a publicly reachable endpoint.
type Hub struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"` // host:port
	TunnelIP string `yaml:"tunnelIP"` // address inside Network.CIDR
}

// Agent is a spoke node that joins the overlay through a hub.
type Agent struct {
	Name     string   `yaml:"name"`
	TunnelIP string   `yaml:"tunnelIP"`
	Hub      string   `yaml:"hub"` // references Hub.Name
	Groups   []string `yaml:"groups,omitempty"`
}

// ACL is a single rule in the network's policy.
//
// From and To are sets of group names. Action is allow or deny. Allow rules
// open the listed (from → to) flows; deny rules override allows for the
// same source/destination intersection. The default policy when no rule
// matches is deny.
type ACL struct {
	From   []string `yaml:"from"`
	To     []string `yaml:"to"`
	Action Action   `yaml:"action"`
}
