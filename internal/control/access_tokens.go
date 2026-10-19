package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultAccessTokenLifetime = 30 * 24 * time.Hour
	maxAccessTokenLifetime     = 365 * 24 * time.Hour
)

func (s *service) CreateAccessToken(ctx context.Context, request *connect.Request[orchv1alpha1.CreateAccessTokenRequest]) (*connect.Response[orchv1alpha1.CreateAccessTokenResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if err := config.ValidateApplicationName(request.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("token name: %w", err))
	}
	if err := config.ValidateApplicationName(request.Msg.GetApplication()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	lifetime := time.Duration(request.Msg.GetTtlSeconds()) * time.Second
	if lifetime == 0 {
		lifetime = defaultAccessTokenLifetime
	}
	if lifetime < time.Minute || lifetime > maxAccessTokenLifetime {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token lifetime must be between 1 minute and 365 days"))
	}
	block, _ := pem.Decode(s.caPEM)
	if block == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("controller CA is unavailable"))
	}
	id, secret, secretHash, err := security.NewAccessTokenSecret(s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := s.now().UTC()
	token := model.AccessToken{
		ID: id, Name: request.Msg.GetName(), Application: request.Msg.GetApplication(), ReadOnly: request.Msg.GetReadOnly(),
		SecretHash: secretHash, CreatedAt: now, ExpiresAt: now.Add(lifetime),
	}
	if err := s.state.CreateAccessToken(ctx, token); errors.Is(err, store.ErrAccessTokenExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("access token %s already exists; delete it first or pick another name", token.Name))
	} else if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	fingerprint := sha256.Sum256(block.Bytes)
	address := s.cluster.Endpoint + "#sha256:" + hex.EncodeToString(fingerprint[:])
	return connect.NewResponse(&orchv1alpha1.CreateAccessTokenResponse{
		Token: accessTokenMessage(token), Value: security.EncodeAccessToken(address, id, secret),
	}), nil
}

func (s *service) ListAccessTokens(ctx context.Context, request *connect.Request[orchv1alpha1.ListAccessTokensRequest]) (*connect.Response[orchv1alpha1.ListAccessTokensResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	tokens, err := s.state.AccessTokens(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list access tokens"))
	}
	response := &orchv1alpha1.ListAccessTokensResponse{}
	for _, token := range tokens {
		response.Tokens = append(response.Tokens, accessTokenMessage(token))
	}
	return connect.NewResponse(response), nil
}

func (s *service) DeleteAccessToken(ctx context.Context, request *connect.Request[orchv1alpha1.DeleteAccessTokenRequest]) (*connect.Response[orchv1alpha1.DeleteAccessTokenResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if err := config.ValidateApplicationName(request.Msg.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("token name: %w", err))
	}
	changed, err := s.state.DeleteAccessToken(ctx, request.Msg.GetName())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("delete access token"))
	}
	return connect.NewResponse(&orchv1alpha1.DeleteAccessTokenResponse{Changed: changed}), nil
}

// ExchangeAccessToken trades a valid access token for a short-lived deployer
// certificate, or a viewer certificate for a read-only token. Deleting or
// expiring the token stops new exchanges; certificates already issued last
// at most their 15 minutes.
func (s *credentialService) ExchangeAccessToken(ctx context.Context, request *connect.Request[orchv1alpha1.ExchangeAccessTokenRequest]) (*connect.Response[orchv1alpha1.ExchangeAccessTokenResponse], error) {
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	csrDER := request.Msg.GetCertificateRequestDer()
	if len(csrDER) == 0 || len(csrDER) > maxCSRBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("certificate_request_der is required"))
	}
	rejected := connect.NewError(connect.CodeUnauthenticated, errors.New("access token is invalid, expired or deleted; create a new one with orch token create"))
	id, secret, ok := security.SplitAccessTokenCredential(request.Msg.GetToken())
	if !ok {
		return nil, rejected
	}
	token, err := s.state.AccessToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, rejected
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read access token"))
	}
	now := s.now()
	if subtle.ConstantTimeCompare([]byte(token.SecretHash), []byte(security.AccessTokenSecretHash(secret))) != 1 || !now.Before(token.ExpiresAt) {
		return nil, rejected
	}
	certificatePEM, notAfter, err := security.SignApplicationCertificate(s.caPEM, s.caKey, s.cluster.ID, token.Application, token.ReadOnly, csrDER, now, s.random)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_ = s.state.TouchAccessToken(ctx, token.ID, now)
	return connect.NewResponse(&orchv1alpha1.ExchangeAccessTokenResponse{
		Application: token.Application, ReadOnly: token.ReadOnly, CertificatePem: certificatePEM, ExpiresAt: timestamppb.New(notAfter),
	}), nil
}

func accessTokenMessage(token model.AccessToken) *orchv1alpha1.AccessToken {
	message := &orchv1alpha1.AccessToken{
		Name: token.Name, Application: token.Application, ReadOnly: token.ReadOnly,
		CreatedAt: timestamppb.New(token.CreatedAt), ExpiresAt: timestamppb.New(token.ExpiresAt),
	}
	if !token.LastUsedAt.IsZero() {
		message.LastUsedAt = timestamppb.New(token.LastUsedAt)
	}
	return message
}
