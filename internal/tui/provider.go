package tui

import (
	"bytes"
	"context"
	"fmt"
	"unicode/utf8"

	orchv1alpha1 "change.codes/torsten/better-swarm/gen/orch/v1alpha1"
	controlclient "change.codes/torsten/better-swarm/internal/client"
)

const maxLogBytes = 512 << 10

type Provider interface {
	GetCluster(context.Context) (*orchv1alpha1.Cluster, error)
	ListApplications(context.Context) ([]*orchv1alpha1.ApplicationSummary, error)
	GetApplication(context.Context, string) (*orchv1alpha1.Application, error)
	ListNodes(context.Context) ([]*orchv1alpha1.Node, error)
	ListEvents(context.Context, uint64, uint32) (*orchv1alpha1.ListEventsResponse, error)
	ListRecentEvents(context.Context, uint32) (*orchv1alpha1.ListRecentEventsResponse, error)
	UpdateNode(context.Context, *orchv1alpha1.UpdateNodeRequest) (*orchv1alpha1.UpdateNodeResponse, error)
	LoadLogs(context.Context, string, string, uint32, uint32) ([]LogLine, error)
	LoadCronLogs(context.Context, string, string, uint32) ([]LogLine, error)
}

type LogLine struct {
	Stream string
	Text   string
}

type ClientProvider struct{ Client *controlclient.Client }

func (p ClientProvider) GetCluster(ctx context.Context) (*orchv1alpha1.Cluster, error) {
	return p.Client.GetCluster(ctx)
}

func (p ClientProvider) ListApplications(ctx context.Context) ([]*orchv1alpha1.ApplicationSummary, error) {
	return p.Client.ListApplications(ctx)
}

func (p ClientProvider) GetApplication(ctx context.Context, name string) (*orchv1alpha1.Application, error) {
	return p.Client.GetApplication(ctx, name)
}

func (p ClientProvider) ListNodes(ctx context.Context) ([]*orchv1alpha1.Node, error) {
	return p.Client.ListNodes(ctx)
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

func (p ClientProvider) LoadLogs(ctx context.Context, application, service string, replica, tail uint32) ([]LogLine, error) {
	stream, err := p.Client.TailLogs(ctx, application, service, replica, tail)
	if err != nil {
		return nil, err
	}
	return collectLogs(stream.Receive, func() ([]byte, string) {
		message := stream.Msg()
		if message == nil {
			return nil, ""
		}
		return message.GetData(), enumLabel(message.GetStream().String())
	}, stream.Err, stream.Close)
}

func (p ClientProvider) LoadCronLogs(ctx context.Context, application, job string, tail uint32) ([]LogLine, error) {
	runs, err := p.Client.ListCronRuns(ctx, application, job, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return []LogLine{}, nil
	}
	stream, err := p.Client.TailCronLogs(ctx, application, job, runs[0].GetId(), tail)
	if err != nil {
		return nil, err
	}
	return collectLogs(stream.Receive, func() ([]byte, string) {
		message := stream.Msg()
		if message == nil {
			return nil, ""
		}
		return message.GetData(), enumLabel(message.GetStream().String())
	}, stream.Err, stream.Close)
}

func collectLogs(receive func() bool, message func() ([]byte, string), streamErr func() error, closeStream func() error) ([]LogLine, error) {
	defer closeStream()
	var result []LogLine
	retained := 0
	for receive() {
		data, stream := message()
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
		result = append(result, LogLine{Stream: stream, Text: sanitize(string(data))})
		retained += len(data)
	}
	if err := streamErr(); err != nil {
		return nil, fmt.Errorf("read workload logs: %w", err)
	}
	return result, nil
}
