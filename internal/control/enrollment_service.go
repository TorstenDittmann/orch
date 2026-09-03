package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	"change.codes/torsten/better-swarm/gen/orch/v1alpha1/orchv1alpha1connect"
	"change.codes/torsten/better-swarm/internal/config"
	"change.codes/torsten/better-swarm/internal/model"
	"change.codes/torsten/better-swarm/internal/security"
	"change.codes/torsten/better-swarm/internal/store"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type enrollmentService struct {
	orchv1alpha1connect.UnimplementedEnrollmentServiceHandler
	state      *store.Store
	cluster    model.Cluster
	caPEM      []byte
	caKey      []byte
	now        func() time.Time
	random     io.Reader
	reconciler *Reconciler
}

func newEnrollmentService(state *store.Store, cluster model.Cluster, caPEM, caKey []byte, now func() time.Time) *enrollmentService {
	if now == nil {
		now = time.Now
	}
	return &enrollmentService{state: state, cluster: cluster, caPEM: append([]byte(nil), caPEM...),
		caKey: append([]byte(nil), caKey...), now: now, random: rand.Reader}
}

func (s *enrollmentService) EnrollAgent(ctx context.Context, request *connect.Request[orchv1alpha1.EnrollmentServiceEnrollAgentRequest]) (*connect.Response[orchv1alpha1.EnrollmentServiceEnrollAgentResponse], error) {
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if !idempotencyKeyPattern.MatchString(request.Msg.GetJoinId()) || !idempotencyKeyPattern.MatchString(request.Msg.GetIdempotencyKey()) ||
		len(request.Msg.GetSecret()) != 32 {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	name := request.Msg.GetName()
	if err := config.ValidateApplicationName(name); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	csrDER := request.Msg.GetCertificateRequestDer()
	if len(csrDER) == 0 || len(csrDER) > maxCSRBytes {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	secretDigest := sha256.Sum256(request.Msg.GetSecret())
	secretHash := hex.EncodeToString(secretDigest[:])
	now := s.now().UTC()
	if err := s.state.ValidateJoinToken(ctx, request.Msg.GetJoinId(), secretHash, name, now); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	publicKeyHash, err := security.AgentCSRPublicKeyHash(csrDER)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	nodeID := newNodeID(s.cluster.ID, publicKeyHash)
	hasher := sha256.New()
	hasher.Write([]byte(request.Msg.GetJoinId()))
	hasher.Write([]byte{0})
	hasher.Write([]byte(name))
	hasher.Write([]byte{0})
	hasher.Write(csrDER)
	requestHash := hex.EncodeToString(hasher.Sum(nil))
	if err := s.state.ValidateJoinTokenEnrollment(ctx, request.Msg.GetJoinId(), secretHash, name, requestHash, request.Msg.GetIdempotencyKey(), now); err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	// Signing happens only after both the cheap token check and the exact
	// request/replay binding have succeeded.
	signed, err := security.SignAgentCertificate(s.caPEM, s.caKey, s.cluster.ID, nodeID, csrDER, now, s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("sign agent identity"))
	}
	result, err := s.state.EnrollAgentWithJoin(ctx, request.Msg.GetJoinId(), secretHash, model.EnrollAgent{
		NodeID: nodeID, Name: name, PublicKeyHash: signed.PublicKeyHash,
		CertificateSerial: signed.Serial, CertificatePEM: signed.CertificatePEM,
		CertificateNotAfter: signed.NotAfter, ClusterID: s.cluster.ID,
		ControllerEndpoint: s.cluster.Endpoint, CACertificatePEM: s.caPEM,
		IdempotencyKey: request.Msg.GetIdempotencyKey(), RequestHash: requestHash, CreatedAt: now,
	})
	if errors.Is(err, store.ErrJoinTokenUnavailable) {
		return nil, connect.NewError(connect.CodeUnauthenticated, store.ErrJoinTokenUnavailable)
	}
	if errors.Is(err, store.ErrIdempotencyConflict) || errors.Is(err, store.ErrNodeAlreadyEnrolled) || errors.Is(err, store.ErrNodeRemoved) {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	if errors.Is(err, store.ErrNodeLimitReached) {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("persist joined agent enrollment"))
	}
	if s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish initial agent assignment"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.EnrollmentServiceEnrollAgentResponse{
		NodeId: result.NodeID, NodeName: result.NodeName, ClusterId: result.ClusterID,
		ControllerEndpoint: result.ControllerEndpoint, CaCertificatePem: append([]byte(nil), result.CACertificatePEM...),
		AgentCertificatePem: append([]byte(nil), result.AgentCertificatePEM...), ExpiresAt: timestamppb.New(result.ExpiresAt),
		Changed: result.Changed, EventSequence: result.EventSequence,
	}), nil
}
