// Package controlplane implements the management server: agent
// registration, heartbeats and an in-memory cache of the network spec
// pulled from Git.
package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ilyapiatykh/seine/internal/store"
)

// agentCtxKey is the context key under which an authenticated agent is
// attached to a request by the auth interceptor.
type agentCtxKey struct{}

// AgentFromContext returns the agent that authenticated the request, or
// nil if the request was unauthenticated.
func AgentFromContext(ctx context.Context) *store.Agent {
	v, _ := ctx.Value(agentCtxKey{}).(*store.Agent)
	return v
}

// withAgent attaches an authenticated agent to a context.
func withAgent(ctx context.Context, a *store.Agent) context.Context {
	return context.WithValue(ctx, agentCtxKey{}, a)
}

// generateAuthToken returns a 32-byte URL-safe random token used as a
// bearer credential. The plain token is given to the agent; only its
// SHA-256 digest is persisted on the server.
func generateAuthToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("controlplane: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// generateAgentID returns an opaque random identifier for a newly
// registered agent.
func generateAgentID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("controlplane: rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// constantTimeEqual reports whether the two strings are equal without
// leaking length-difference information through timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// AuthInterceptor builds a unary server interceptor that enforces bearer
// auth on every method except those listed in skip. The bearer token is
// looked up by SHA-256 hash in store; the matching Agent is attached to
// the request context under AgentFromContext.
func AuthInterceptor(s *store.Store, skip map[string]struct{}) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if _, ok := skip[info.FullMethod]; ok {
			return handler(ctx, req)
		}
		token, err := tokenFromMetadata(ctx)
		if err != nil {
			return nil, err
		}
		agent, err := s.AuthenticateByTokenHash(ctx, store.HashToken(token))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
			}
			return nil, status.Errorf(codes.Internal, "auth lookup: %v", err)
		}
		return handler(withAgent(ctx, agent), req)
	}
}

// tokenFromMetadata extracts a bearer token from the gRPC metadata. It
// accepts either "authorization: Bearer <tok>" (canonical) or
// "x-seine-token: <tok>" for tooling that cannot set the standard header.
func tokenFromMetadata(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	if vals := md.Get("authorization"); len(vals) > 0 {
		const prefix = "bearer "
		v := vals[0]
		if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			return strings.TrimSpace(v[len(prefix):]), nil
		}
	}
	if vals := md.Get("x-seine-token"); len(vals) > 0 {
		return strings.TrimSpace(vals[0]), nil
	}
	return "", status.Error(codes.Unauthenticated, "missing bearer token")
}
