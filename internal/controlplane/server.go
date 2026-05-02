package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/logging"
	"github.com/ilyapiatykh/seine/internal/store"
)

// Config bundles the dependencies of a Server.
type Config struct {
	// Store is the SQLite-backed persistence layer.
	Store *store.Store

	// Spec is the read-only view of the network specification (pulled
	// from Git).
	Spec SpecProvider

	// BootstrapToken is the operator-issued shared secret an agent must
	// present at Register time.
	BootstrapToken string
}

// Server implements cpv1.ControlPlaneServer.
type Server struct {
	cpv1.UnimplementedControlPlaneServer
	cfg Config
}

// NewServer constructs a Server. It does not start a network listener;
// see Server.Register / Server.RegisterReflection.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("controlplane: Store is required")
	}
	if cfg.Spec == nil {
		return nil, errors.New("controlplane: Spec is required")
	}
	if cfg.BootstrapToken == "" {
		return nil, errors.New("controlplane: BootstrapToken is required")
	}
	return &Server{cfg: cfg}, nil
}

// AttachTo wires the gRPC handlers onto srv.
func (s *Server) AttachTo(srv *grpc.Server) {
	cpv1.RegisterControlPlaneServer(srv, s)
}

// SkipAuthMethods is the set of fully-qualified method names that the auth
// interceptor must NOT challenge (Register cannot be authenticated; the
// agent has no token yet).
func SkipAuthMethods() map[string]struct{} {
	return map[string]struct{}{
		cpv1.ControlPlane_Register_FullMethodName: {},
	}
}

// Register handles the initial enrolment of an agent.
func (s *Server) Register(ctx context.Context, req *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	log := logging.FromContext(ctx).With(slog.String("rpc", "Register"))

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetPublicKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "public_key is required")
	}
	if !constantTimeEqual(req.GetBootstrapToken(), s.cfg.BootstrapToken) {
		return nil, status.Error(codes.Unauthenticated, "invalid bootstrap token")
	}

	doc, _, err := s.cfg.Spec.Current()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "spec not yet loaded: %v", err)
	}
	role, hub, _ := resolveRole(doc, req.GetName())
	if role == cpv1.Role_ROLE_UNSPECIFIED {
		return nil, status.Errorf(codes.PermissionDenied,
			"%q is not declared in the network spec", req.GetName())
	}

	endpoint := req.GetEndpoint()
	switch role {
	case cpv1.Role_ROLE_HUB:
		// Hubs must advertise an endpoint so spokes can dial them. If
		// the agent omitted it (running behind orchestrator that fills
		// it in later), fall back to the spec's endpoint.
		if endpoint == "" && hub != nil {
			endpoint = hub.Endpoint
		}
	}

	id, err := generateAgentID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "id: %v", err)
	}
	tok, err := generateAuthToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "token: %v", err)
	}

	now := time.Now().UTC()
	a := store.Agent{
		ID:            id,
		Name:          req.GetName(),
		PublicKey:     req.GetPublicKey(),
		Endpoint:      endpoint,
		AuthTokenHash: store.HashToken(tok),
		CreatedAt:     now,
		LastSeenAt:    now,
	}
	if err := s.cfg.Store.UpsertAgent(ctx, a); err != nil {
		return nil, status.Errorf(codes.Internal, "persist: %v", err)
	}

	log.Info("agent registered",
		slog.String("name", a.Name),
		slog.String("role", role.String()),
		slog.String("agent_version", req.GetAgentVersion()),
	)
	return &cpv1.RegisterResponse{AuthToken: tok}, nil
}

// Heartbeat updates the caller's runtime state and returns the registry of
// peers for reconciliation.
func (s *Server) Heartbeat(ctx context.Context, req *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	caller := AgentFromContext(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "auth context missing")
	}

	doc, commit, err := s.cfg.Spec.Current()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "spec not yet loaded: %v", err)
	}

	now := time.Now().UTC()
	if err := s.cfg.Store.UpdateHeartbeat(ctx, caller.ID, req.GetEndpoint(), now); err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat: %v", err)
	}

	all, err := s.cfg.Store.ListAgents(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}

	peers := make([]*cpv1.PeerInfo, 0, len(all))
	for _, a := range all {
		role, _, _ := resolveRole(doc, a.Name)
		if role == cpv1.Role_ROLE_UNSPECIFIED {
			// Stale registration whose name was removed from the spec.
			// We hide it from peers; operator should garbage-collect.
			continue
		}
		// Use the just-updated endpoint for the caller.
		ep := a.Endpoint
		if a.ID == caller.ID && req.GetEndpoint() != "" {
			ep = req.GetEndpoint()
		}
		peers = append(peers, &cpv1.PeerInfo{
			Name:      a.Name,
			PublicKey: a.PublicKey,
			Endpoint:  ep,
			TunnelIp:  tunnelIPFor(doc, a.Name),
			LastSeen:  timestamppb.New(a.LastSeenAt),
			Role:      role,
		})
	}

	return &cpv1.HeartbeatResponse{
		SpecCommit: commit,
		Peers:      peers,
	}, nil
}

// describe returns a short status string for diagnostics.
func (s *Server) describe(ctx context.Context) string {
	all, err := s.cfg.Store.ListAgents(ctx)
	if err != nil {
		return fmt.Sprintf("controlplane: list error: %v", err)
	}
	return fmt.Sprintf("controlplane: %d registered agents", len(all))
}
