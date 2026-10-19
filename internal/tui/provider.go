package tui

import (
	"bytes"
	"context"
	"fmt"
	"unicode/utf8"

	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	controlclient "github.com/TorstenDittmann/orch/internal/client"
)

const maxLogBytes = 512 << 10

type Provider interface {
	ListDeployments(context.Context, string, uint64, uint32) (*orchv1alpha1.ListDeploymentsResponse, error)
	CompareDeployments(context.Context, string, string, string) (*orchv1alpha1.CompareDeploymentsResponse, error)
	RollbackApplication(context.Context, *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error)
	GetClusterStatus(context.Context) (*orchv1alpha1.Cluster, *orchv1alpha1.ControllerBackup, error)
	ListApplications(context.Context) ([]*orchv1alpha1.ApplicationSummary, error)
	ListRuntimes(context.Context, uint32) (*orchv1alpha1.ListRuntimesResponse, error)
	GetApplication(context.Context, string) (*orchv1alpha1.Application, error)
	ListNodes(context.Context) ([]*orchv1alpha1.Node, error)
	GetNodeUsage(context.Context, string) (*orchv1alpha1.GetNodeUsageResponse, error)
	ListEvents(context.Context, uint64, uint32) (*orchv1alpha1.ListEventsResponse, error)
	ListRecentEvents(context.Context, uint32) (*orchv1alpha1.ListRecentEventsResponse, error)
	UpdateNode(context.Context, *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error)
	LoadLogs(context.Context, string, string, uint32, uint32, string) (LogBatch, error)
	LoadPreviousLogs(context.Context, string, string, uint32, uint32) (LogBatch, error)
	LoadCronLogs(context.Context, string, string, string, uint32, string) (LogBatch, error)
}

type LogLine struct {
	Stream string
	Text   string
}

type LogBatch struct {
	Lines       []LogLine
	NextCursor  string
	CursorReset bool
	Exited      bool
}

type ClientProvider struct{ Client *controlclient.Client }

func (p ClientProvider) GetClusterStatus(ctx context.Context) (*orchv1alpha1.Cluster, *orchv1alpha1.ControllerBackup, error) {
	return p.Client.GetClusterStatus(ctx)
}

func (p ClientProvider) ListApplications(ctx context.Context) ([]*orchv1alpha1.ApplicationSummary, error) {
	return p.Client.ListApplications(ctx)
}

func (p ClientProvider) ListRuntimes(ctx context.Context, limit uint32) (*orchv1alpha1.ListRuntimesResponse, error) {
	return p.Client.ListRuntimes(ctx, limit)
}

func (p ClientProvider) GetApplication(ctx context.Context, name string) (*orchv1alpha1.Application, error) {
	return p.Client.GetApplication(ctx, name)
}

func (p ClientProvider) ListNodes(ctx context.Context) ([]*orchv1alpha1.Node, error) {
	return p.Client.ListNodes(ctx)
}

func (p ClientProvider) GetNodeUsage(ctx context.Context, name string) (*orchv1alpha1.GetNodeUsageResponse, error) {
	return p.Client.GetNodeUsage(ctx, name)
}

func (p ClientProvider) ListEvents(ctx context.Context, after uint64, limit uint32) (*orchv1alpha1.ListEventsResponse, error) {
	return p.Client.ListEvents(ctx, after, limit)
}

func (p ClientProvider) ListRecentEvents(ctx context.Context, limit uint32) (*orchv1alpha1.ListRecentEventsResponse, error) {
	return p.Client.ListRecentEvents(ctx, limit)
}

func (p ClientProvider) UpdateNode(ctx context.Context, request *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error) {
	return p.Client.UpdateNode(ctx, request)
}

func (p ClientProvider) LoadLogs(ctx context.Context, application, service string, replica, tail uint32, cursor string) (LogBatch, error) {
	stream, err := p.Client.TailLogsAfter(ctx, application, service, replica, tail, cursor)
	if err != nil {
		return LogBatch{}, err
	}
	return collectLogs(stream.Receive, func() ([]byte, string, string, bool, bool) {
		message := stream.Msg()
		if message == nil {
			return nil, "", "", false, false
		}
		return message.GetData(), enumLabel(message.GetStream().String()), message.GetNextCursor(), message.GetCursorReset(), message.GetRuntimeExited()
	}, stream.Err, stream.Close)
}

func (p ClientProvider) LoadPreviousLogs(ctx context.Context, application, service string, replica, tail uint32) (LogBatch, error) {
	stream, err := p.Client.TailPreviousLogs(ctx, application, service, replica, tail)
	if err != nil {
		return LogBatch{}, err
	}
	return collectLogs(stream.Receive, func() ([]byte, string, string, bool, bool) {
		message := stream.Msg()
		if message == nil {
			return nil, "", "", false, false
		}
		return message.GetData(), enumLabel(message.GetStream().String()), message.GetNextCursor(), message.GetCursorReset(), message.GetRuntimeExited()
	}, stream.Err, stream.Close)
}

func (p ClientProvider) LoadCronLogs(ctx context.Context, application, job, runID string, tail uint32, cursor string) (LogBatch, error) {
	stream, err := p.Client.TailCronLogsAfter(ctx, application, job, runID, tail, cursor)
	if err != nil {
		return LogBatch{}, err
	}
	return collectLogs(stream.Receive, func() ([]byte, string, string, bool, bool) {
		message := stream.Msg()
		if message == nil {
			return nil, "", "", false, false
		}
		return message.GetData(), enumLabel(message.GetStream().String()), message.GetNextCursor(), message.GetCursorReset(), message.GetRuntimeExited()
	}, stream.Err, stream.Close)
}

func collectLogs(receive func() bool, message func() ([]byte, string, string, bool, bool), streamErr func() error, closeStream func() error) (LogBatch, error) {
	defer closeStream()
	var result LogBatch
	retained := 0
	for receive() {
		data, stream, cursor, reset, exited := message()
		if cursor != "" {
			result.NextCursor = cursor
		}
		result.CursorReset = result.CursorReset || reset
		result.Exited = result.Exited || exited
		if len(data) == 0 {
			continue
		}
		remaining := maxLogBytes - retained
		if remaining <= 0 {
			break
		}
		if len(data) > remaining {
			data = data[:remaining]
		}
		data = bytes.ToValidUTF8(data, []byte(string(utf8.RuneError)))
		result.Lines = append(result.Lines, LogLine{Stream: stream, Text: sanitize(string(data))})
		retained += len(data)
	}
	if err := streamErr(); err != nil {
		return LogBatch{}, fmt.Errorf("read workload logs: %w", err)
	}
	return result, nil
}

func (p ClientProvider) ListDeployments(ctx context.Context, app string, before uint64, limit uint32) (*orchv1alpha1.ListDeploymentsResponse, error) {
	return p.Client.ListDeployments(ctx, app, before, limit)
}
func (p ClientProvider) CompareDeployments(ctx context.Context, app, from, to string) (*orchv1alpha1.CompareDeploymentsResponse, error) {
	return p.Client.CompareDeployments(ctx, app, from, to)
}
func (p ClientProvider) RollbackApplication(ctx context.Context, request *orchv1alpha1.RollbackApplicationRequest) (*orchv1alpha1.ApplyApplicationResponse, error) {
	return p.Client.RollbackApplication(ctx, request)
}
