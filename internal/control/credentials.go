package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"github.com/TorstenDittmann/orch/internal/security"
	"github.com/TorstenDittmann/orch/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var githubRepositoryPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})/[a-z0-9._-]{1,100}$`)

// credentialService issues deployer certificates to GitHub Actions runs that
// match a trust rule. It is served without client authentication.
type credentialService struct {
	orchv1alpha1connect.UnimplementedCredentialServiceHandler
	state   *store.Store
	cluster model.Cluster
	caPEM   []byte
	caKey   []byte
	now     func() time.Time
	random  io.Reader
	github  *githubVerifier
}

func newCredentialService(state *store.Store, cluster model.Cluster, caPEM, caKey []byte, now func() time.Time) *credentialService {
	if now == nil {
		now = time.Now
	}
	return &credentialService{state: state, cluster: cluster, caPEM: append([]byte(nil), caPEM...),
		caKey: append([]byte(nil), caKey...), now: now, random: rand.Reader, github: newGitHubVerifier(now)}
}

func (s *credentialService) ExchangeGitHubToken(ctx context.Context, request *connect.Request[orchv1alpha1.ExchangeGitHubTokenRequest]) (*connect.Response[orchv1alpha1.ExchangeGitHubTokenResponse], error) {
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	application := request.Msg.GetApplication()
	if application != "" {
		if err := config.ValidateApplicationName(application); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	csrDER := request.Msg.GetCertificateRequestDer()
	if len(csrDER) == 0 || len(csrDER) > maxCSRBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("certificate_request_der is required"))
	}
	claims, err := s.github.verify(ctx, request.Msg.GetToken(), GitHubAudience(s.cluster.ID))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("GitHub token rejected: %w", err))
	}
	trust, err := s.matchGitHubTrust(ctx, strings.ToLower(claims.Repository), application, claims)
	if err != nil {
		return nil, err
	}
	application = trust.Application
	certificatePEM, notAfter, err := security.SignDeployerCertificate(s.caPEM, s.caKey, s.cluster.ID, application, csrDER, s.now(), s.random, trust.Domains...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&orchv1alpha1.ExchangeGitHubTokenResponse{
		Application: application, CertificatePem: certificatePEM, ExpiresAt: timestamppb.New(notAfter),
	}), nil
}

// githubDeployEvents are the workflow triggers that run the trusted branch's
// own code, so a matching ref means the deploy comes from that branch.
var githubDeployEvents = map[string]bool{
	"push":                true,
	"workflow_dispatch":   true,
	"schedule":            true,
	"repository_dispatch": true,
}

// matchGitHubTrust finds the rule that admits this run. Without an explicit
// application, the repository must be trusted for exactly one.
func (s *credentialService) matchGitHubTrust(ctx context.Context, repository, application string, claims githubClaims) (model.GitHubTrust, error) {
	trusts, err := s.state.GitHubTrusts(ctx)
	if err != nil {
		return model.GitHubTrust{}, connect.NewError(connect.CodeInternal, errors.New("read GitHub trust"))
	}
	var candidates []model.GitHubTrust
	for _, trust := range trusts {
		if trust.Repository == repository && (application == "" || trust.Application == application) {
			candidates = append(candidates, trust)
		}
	}
	switch {
	case len(candidates) == 0 && application == "":
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is not trusted to deploy; run orch github trust %s --app APP", repository, repository))
	case len(candidates) == 0:
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is not trusted to deploy %s; run orch github trust %s --app %s", repository, application, repository, application))
	case len(candidates) > 1:
		return model.GitHubTrust{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is trusted for several applications; set ORCH_APP to the one to deploy", repository))
	}
	trust := candidates[0]
	if !githubDeployEvents[claims.EventName] {
		// Events such as pull_request_target and workflow_run carry the base
		// branch ref but often build untrusted pull request code.
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s runs may not deploy; deploy from push, workflow_dispatch, schedule or repository_dispatch", claims.EventName))
	}
	if claims.Ref != "refs/heads/"+trust.Branch {
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s may deploy %s only from branch %s, not %s", repository, trust.Application, trust.Branch, claims.Ref))
	}
	if trust.Environment != "" && claims.Environment != trust.Environment {
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s may deploy %s only from GitHub environment %s", repository, trust.Application, trust.Environment))
	}
	// Names can be taken over after a repository or its owner is renamed or
	// deleted, so the rule is bound to GitHub's IDs on the first deploy.
	if trust.RepositoryID == "" {
		if trust, err = s.state.BindGitHubTrust(ctx, trust.Repository, trust.Application, claims.RepositoryID, claims.RepositoryOwnerID); err != nil {
			return model.GitHubTrust{}, connect.NewError(connect.CodeInternal, errors.New("bind GitHub trust"))
		}
	}
	if trust.RepositoryID != claims.RepositoryID || trust.RepositoryOwnerID != claims.RepositoryOwnerID {
		return model.GitHubTrust{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s is not the repository trusted to deploy %s; it was deleted or renamed and the name now belongs to another one. If this is intended, run orch github trust %s --app %s again", repository, trust.Application, repository, trust.Application))
	}
	return trust, nil
}

func (s *service) SetGitHubTrust(ctx context.Context, request *connect.Request[orchv1alpha1.SetGitHubTrustRequest]) (*connect.Response[orchv1alpha1.SetGitHubTrustResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	repository, application, err := githubTrustKey(request.Msg.GetRepository(), request.Msg.GetApplication())
	if err != nil {
		return nil, err
	}
	branch, environment := request.Msg.GetBranch(), request.Msg.GetEnvironment()
	if !validGitHubName(branch) || (environment != "" && !validGitHubName(environment)) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("branch is required, and branch and environment must be printable names of at most 255 bytes"))
	}
	domains, err := config.NormalizeDomainLimits(request.Msg.GetDomains())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	trust, changed, err := s.state.SetGitHubTrust(ctx, model.GitHubTrust{
		Repository: repository, Application: application, Branch: branch, Environment: environment, CreatedAt: s.now(), Domains: domains,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store GitHub trust"))
	}
	block, _ := pem.Decode(s.caPEM)
	if block == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("controller CA is unavailable"))
	}
	fingerprint := sha256.Sum256(block.Bytes)
	return connect.NewResponse(&orchv1alpha1.SetGitHubTrustResponse{
		Trust: githubTrustMessage(trust), Changed: changed,
		ControllerEndpoint: s.cluster.Endpoint, ClusterId: s.cluster.ID, CaFingerprint: fingerprint[:],
	}), nil
}

func (s *service) ListGitHubTrusts(ctx context.Context, request *connect.Request[orchv1alpha1.ListGitHubTrustsRequest]) (*connect.Response[orchv1alpha1.ListGitHubTrustsResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	trusts, err := s.state.GitHubTrusts(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("list GitHub trust"))
	}
	response := &orchv1alpha1.ListGitHubTrustsResponse{}
	for _, trust := range trusts {
		response.Trusts = append(response.Trusts, githubTrustMessage(trust))
	}
	return connect.NewResponse(response), nil
}

func (s *service) DeleteGitHubTrust(ctx context.Context, request *connect.Request[orchv1alpha1.DeleteGitHubTrustRequest]) (*connect.Response[orchv1alpha1.DeleteGitHubTrustResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	repository, application, err := githubTrustKey(request.Msg.GetRepository(), request.Msg.GetApplication())
	if err != nil {
		return nil, err
	}
	changed, err := s.state.DeleteGitHubTrust(ctx, repository, application)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("delete GitHub trust"))
	}
	return connect.NewResponse(&orchv1alpha1.DeleteGitHubTrustResponse{Changed: changed}), nil
}

func githubTrustKey(repository, application string) (string, string, error) {
	repository = strings.ToLower(strings.TrimSpace(repository))
	if !githubRepositoryPattern.MatchString(repository) {
		return "", "", connect.NewError(connect.CodeInvalidArgument, errors.New("repository must be OWNER/NAME"))
	}
	if err := config.ValidateApplicationName(application); err != nil {
		return "", "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	return repository, application, nil
}

func validGitHubName(value string) bool {
	return value != "" && len(value) <= 255 && strings.IndexFunc(value, func(r rune) bool { return !unicode.IsPrint(r) || unicode.IsSpace(r) }) < 0
}

func githubTrustMessage(trust model.GitHubTrust) *orchv1alpha1.GitHubTrust {
	return &orchv1alpha1.GitHubTrust{
		Repository: trust.Repository, Application: trust.Application, Branch: trust.Branch,
		Environment: trust.Environment, CreatedAt: timestamppb.New(trust.CreatedAt), Domains: append([]string(nil), trust.Domains...),
	}
}
