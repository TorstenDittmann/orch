package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/config"
	"github.com/TorstenDittmann/orch/internal/model"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *service) SetMCPDomain(ctx context.Context, request *connect.Request[orchv1alpha1.SetMCPDomainRequest]) (*connect.Response[orchv1alpha1.SetMCPDomainResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(request.Msg.GetDomain()), "."))
	if domain != "" && s.mcp == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this controller does not serve remote MCP; check the controller log, or its --mcp-listen setting"))
	}
	if domain != "" {
		if config.IsWildcardDomain(domain) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the MCP domain must be one hostname, not a wildcard"))
		}
		if err := config.ValidateDomain(domain); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("MCP domain: %w", err))
		}
		owner, err := s.applicationRoutingDomain(ctx, domain)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("application %s already routes %s; choose another domain for MCP", owner, domain))
		}
	}
	changed, err := s.state.SetMCPDomain(ctx, domain)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("store MCP domain"))
	}
	if changed && s.reconciler != nil {
		if err := s.reconciler.Reconcile(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("publish the MCP route"))
		}
	}
	return connect.NewResponse(&orchv1alpha1.SetMCPDomainResponse{Changed: changed}), nil
}

// applicationRoutingDomain names the application that routes domain, if any.
func (s *service) applicationRoutingDomain(ctx context.Context, domain string) (string, error) {
	applications, err := s.state.Applications(ctx, false)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, errors.New("list applications"))
	}
	for _, application := range applications {
		if application.DeletedAt != nil {
			continue
		}
		var manifest config.Manifest
		if json.Unmarshal(application.CanonicalManifest, &manifest) != nil {
			continue
		}
		for _, service := range manifest.Services {
			if service.HTTP == nil {
				continue
			}
			for _, key := range service.HTTP.RouteKeys() {
				if key == domain || strings.HasPrefix(key, domain+"/") {
					return application.Name, nil
				}
			}
		}
	}
	return "", nil
}

// requireNotMCPDomain keeps applications off the remote MCP server's domain.
func (s *service) requireNotMCPDomain(ctx context.Context, manifest *config.Manifest) error {
	domain, err := s.state.MCPDomain(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, errors.New("read MCP domain"))
	}
	if domain == "" {
		return nil
	}
	for name, service := range manifest.Services {
		if service.HTTP == nil {
			continue
		}
		for _, key := range service.HTTP.RouteKeys() {
			if key == domain || strings.HasPrefix(key, domain+"/") {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("services.%s.http: %s serves orch's remote MCP server; turn it off with orch mcp expose --off to use it", name, domain))
			}
		}
	}
	return nil
}

func (s *service) GetMCPDomain(ctx context.Context, request *connect.Request[orchv1alpha1.GetMCPDomainRequest]) (*connect.Response[orchv1alpha1.GetMCPDomainResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	domain, err := s.state.MCPDomain(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read MCP domain"))
	}
	return connect.NewResponse(&orchv1alpha1.GetMCPDomainResponse{Domain: domain}), nil
}

func (s *service) ApproveMCPConnection(ctx context.Context, request *connect.Request[orchv1alpha1.ApproveMCPConnectionRequest]) (*connect.Response[orchv1alpha1.ApproveMCPConnectionResponse], error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if principal, _ := ctx.Value(principalKey{}).(authenticatedPrincipal); principal.inProcess {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("an MCP connection cannot approve another"))
	}
	if err := rejectUnknownFields(request.Msg); err != nil {
		return nil, err
	}
	if s.mcp == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this controller does not serve remote MCP"))
	}
	access := request.Msg.GetAccess()
	switch access {
	case "":
		access = model.MCPAccessRead
	case model.MCPAccessRead, model.MCPAccessDeploy, model.MCPAccessDestructive:
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("access must be read, deploy or destructive"))
	}
	application := request.Msg.GetApplication()
	if application != "" {
		if err := config.ValidateApplicationName(application); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	lifetime := time.Duration(request.Msg.GetTtlSeconds()) * time.Second
	if lifetime == 0 {
		lifetime = mcpDefaultGrantTTL
	}
	if lifetime < time.Hour || lifetime > maxAccessTokenLifetime {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("connection lifetime must be between 1 hour and 365 days"))
	}
	if normalizeMCPCode(request.Msg.GetCode()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the code looks like ABCD-EFGH; copy it from the page the assistant opened"))
	}
	if domain, err := s.state.MCPDomain(ctx); err != nil || domain == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("remote MCP is off; turn it on with orch mcp expose DOMAIN"))
	}
	if err := s.state.DeleteExpiredMCPGrants(ctx, s.now()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("remove expired MCP connections"))
	}
	grant, redirectHost, err := s.mcp.approve(ctx, request.Msg.GetCode(), access, application, lifetime)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&orchv1alpha1.ApproveMCPConnectionResponse{Connection: mcpGrantMessage(grant), RedirectHost: redirectHost}), nil
}

func mcpGrantMessage(grant model.MCPGrant) *orchv1alpha1.AccessToken {
	message := &orchv1alpha1.AccessToken{
		Name: grant.Name, Application: grant.Application, ReadOnly: grant.Access == model.MCPAccessRead,
		Administrator: grant.Application == "" && grant.Access != model.MCPAccessRead,
		CreatedAt:     timestamppb.New(grant.CreatedAt), ExpiresAt: timestamppb.New(grant.ExpiresAt),
		McpAccess: grant.Access, McpClient: grant.ClientName,
	}
	if !grant.LastUsedAt.IsZero() {
		message.LastUsedAt = timestamppb.New(grant.LastUsedAt)
	}
	return message
}
