package controlplane_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/controlplane"
	"github.com/ilyapiatykh/seine/internal/spec"
	"github.com/ilyapiatykh/seine/internal/store"
)

type stubSpec struct {
	doc    *spec.Document
	commit string
	err    error
}

func (s *stubSpec) Current() (*spec.Document, string, error) {
	return s.doc, s.commit, s.err
}

const testBootstrapToken = "secret-bootstrap-1234"

func goodSpec(t *testing.T) *spec.Document {
	t.Helper()
	d := &spec.Document{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   spec.Metadata{Name: "test-net"},
		Spec: spec.Network{
			CIDR: "100.64.0.0/10",
			Hubs: []spec.Hub{{Name: "hub1", Endpoint: "hub1.example.com:51820", TunnelIP: "100.64.0.1"}},
			Agents: []spec.Agent{
				{Name: "spoke-a", TunnelIP: "100.64.1.1", Hub: "hub1", Groups: []string{"g"}},
				{Name: "spoke-b", TunnelIP: "100.64.1.2", Hub: "hub1", Groups: []string{"g"}},
			},
			Groups: []string{"g"},
			ACLs:   []spec.ACL{{From: []string{"g"}, To: []string{"g"}, Action: spec.ActionAllow}},
		},
	}
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("test spec is invalid: %v", err)
	}
	return d
}

// fixture wires a Server behind a bufconn listener and returns a connected
// gRPC client plus a teardown func.
type fixture struct {
	t        *testing.T
	client   cpv1.ControlPlaneClient
	conn     *grpc.ClientConn
	srv      *grpc.Server
	store    *store.Store
	provider *stubSpec
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	provider := &stubSpec{doc: goodSpec(t), commit: "deadbeef"}

	cps, err := controlplane.NewServer(controlplane.Config{
		Store:          st,
		Spec:           provider,
		BootstrapToken: testBootstrapToken,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(controlplane.AuthInterceptor(st, controlplane.SkipAuthMethods())),
	)
	cps.AttachTo(srv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("srv.Serve: %v", err)
		}
	}()
	t.Cleanup(srv.Stop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &fixture{
		t:        t,
		client:   cpv1.NewControlPlaneClient(conn),
		conn:     conn,
		srv:      srv,
		store:    st,
		provider: provider,
	}
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func TestRegister_OK(t *testing.T) {
	fx := newFixture(t)
	resp, err := fx.client.Register(context.Background(), &cpv1.RegisterRequest{
		Name:           "spoke-a",
		BootstrapToken: testBootstrapToken,
		PublicKey:      "pubkey-a",
		AgentVersion:   "test/0.0",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.AuthToken == "" {
		t.Fatal("auth token missing")
	}

	// Verify it persisted and the token authenticates us.
	got, err := fx.store.GetAgentByName(context.Background(), "spoke-a")
	if err != nil {
		t.Fatalf("GetAgentByName: %v", err)
	}
	if got.PublicKey != "pubkey-a" {
		t.Errorf("public key not stored: %+v", got)
	}
}

func TestRegister_BadBootstrapToken(t *testing.T) {
	fx := newFixture(t)
	_, err := fx.client.Register(context.Background(), &cpv1.RegisterRequest{
		Name:           "spoke-a",
		BootstrapToken: "wrong",
		PublicKey:      "k",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestRegister_NameNotInSpec(t *testing.T) {
	fx := newFixture(t)
	_, err := fx.client.Register(context.Background(), &cpv1.RegisterRequest{
		Name:           "ghost",
		BootstrapToken: testBootstrapToken,
		PublicKey:      "k",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestRegister_HubInheritsSpecEndpoint(t *testing.T) {
	fx := newFixture(t)
	_, err := fx.client.Register(context.Background(), &cpv1.RegisterRequest{
		Name:           "hub1",
		BootstrapToken: testBootstrapToken,
		PublicKey:      "hub-key",
		// No endpoint provided.
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, _ := fx.store.GetAgentByName(context.Background(), "hub1")
	if got.Endpoint != "hub1.example.com:51820" {
		t.Errorf("hub endpoint not inherited from spec: %q", got.Endpoint)
	}
}

func TestHeartbeat_RequiresAuth(t *testing.T) {
	fx := newFixture(t)
	_, err := fx.client.Heartbeat(context.Background(), &cpv1.HeartbeatRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestHeartbeat_ReturnsPeers(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	// Register two agents.
	regA, err := fx.client.Register(ctx, &cpv1.RegisterRequest{
		Name: "spoke-a", BootstrapToken: testBootstrapToken, PublicKey: "kA",
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	if _, err := fx.client.Register(ctx, &cpv1.RegisterRequest{
		Name: "spoke-b", BootstrapToken: testBootstrapToken, PublicKey: "kB",
	}); err != nil {
		t.Fatalf("Register B: %v", err)
	}
	if _, err := fx.client.Register(ctx, &cpv1.RegisterRequest{
		Name: "hub1", BootstrapToken: testBootstrapToken, PublicKey: "kH",
	}); err != nil {
		t.Fatalf("Register hub: %v", err)
	}

	resp, err := fx.client.Heartbeat(withToken(ctx, regA.AuthToken), &cpv1.HeartbeatRequest{
		Endpoint: "10.0.0.5:7000",
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if resp.SpecCommit != "deadbeef" {
		t.Errorf("spec commit = %q", resp.SpecCommit)
	}
	if got := len(resp.Peers); got != 3 {
		t.Fatalf("peers count = %d, want 3", got)
	}

	byName := map[string]*cpv1.PeerInfo{}
	for _, p := range resp.Peers {
		byName[p.Name] = p
	}
	if byName["spoke-a"].Endpoint != "10.0.0.5:7000" {
		t.Errorf("caller endpoint not reflected: %q", byName["spoke-a"].Endpoint)
	}
	if byName["hub1"].Role != cpv1.Role_ROLE_HUB {
		t.Errorf("hub role: %v", byName["hub1"].Role)
	}
	if byName["spoke-a"].Role != cpv1.Role_ROLE_SPOKE {
		t.Errorf("spoke role: %v", byName["spoke-a"].Role)
	}
	if byName["hub1"].TunnelIp != "100.64.0.1" {
		t.Errorf("hub tunnelIP: %q", byName["hub1"].TunnelIp)
	}
}

func TestHeartbeat_HidesAgentsRemovedFromSpec(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	reg, err := fx.client.Register(ctx, &cpv1.RegisterRequest{
		Name: "spoke-a", BootstrapToken: testBootstrapToken, PublicKey: "k",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Mutate the spec to drop spoke-b — it should not appear in peer list.
	doc := goodSpec(t)
	doc.Spec.Agents = doc.Spec.Agents[:1] // drop spoke-b
	fx.provider.doc = doc

	resp, err := fx.client.Heartbeat(withToken(ctx, reg.AuthToken), &cpv1.HeartbeatRequest{})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	for _, p := range resp.Peers {
		if p.Name == "spoke-b" {
			t.Errorf("spoke-b appeared after removal from spec")
		}
	}
}

func TestHeartbeat_BadTokenRejected(t *testing.T) {
	fx := newFixture(t)
	ctx := withToken(context.Background(), "totally-invalid-token")
	_, err := fx.client.Heartbeat(ctx, &cpv1.HeartbeatRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestRegister_FailsWhenSpecUnavailable(t *testing.T) {
	fx := newFixture(t)
	fx.provider.err = errors.New("not yet loaded")
	_, err := fx.client.Register(context.Background(), &cpv1.RegisterRequest{
		Name: "spoke-a", BootstrapToken: testBootstrapToken, PublicKey: "k",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
}

// ensure HeartbeatRequest's own deadline is honoured.
func TestHeartbeat_RespectsDeadline(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	reg, err := fx.client.Register(ctx, &cpv1.RegisterRequest{
		Name: "spoke-a", BootstrapToken: testBootstrapToken, PublicKey: "k",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	cctx, cancel := context.WithTimeout(withToken(ctx, reg.AuthToken), 5*time.Second)
	defer cancel()
	if _, err := fx.client.Heartbeat(cctx, &cpv1.HeartbeatRequest{}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}
