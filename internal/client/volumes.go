package client

import (
	"context"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

func (c *Client) ListVolumes(ctx context.Context, application, volume string) ([]*orchv1alpha1.VolumeSummary, error) {
	r, err := c.service.ListVolumes(ctx, connect.NewRequest(&orchv1alpha1.ListVolumesRequest{Application: application, Volume: volume}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetVolumes(), nil
}

func (c *Client) BackupVolume(ctx context.Context, application, volume string) (*orchv1alpha1.VolumeBackupInfo, error) {
	r, err := c.service.BackupVolume(ctx, connect.NewRequest(&orchv1alpha1.BackupVolumeRequest{Application: application, Volume: volume}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetBackup(), nil
}

func (c *Client) RestoreVolume(ctx context.Context, application, volume, backup string) (*orchv1alpha1.VolumeBackupInfo, error) {
	r, err := c.service.RestoreVolume(ctx, connect.NewRequest(&orchv1alpha1.RestoreVolumeRequest{Application: application, Volume: volume, Backup: backup}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetPrevious(), nil
}

func (c *Client) DownloadVolumeBackup(ctx context.Context, application, volume, backup string) (*connect.ServerStreamForClient[orchv1alpha1.DownloadVolumeBackupResponse], error) {
	return c.service.DownloadVolumeBackup(ctx, connect.NewRequest(&orchv1alpha1.DownloadVolumeBackupRequest{Application: application, Volume: volume, Backup: backup}))
}

func (c *Client) RestartService(ctx context.Context, application, service string, replica uint32) ([]uint32, error) {
	r, err := c.service.RestartService(ctx, connect.NewRequest(&orchv1alpha1.RestartServiceRequest{Application: application, Service: service, Replica: replica}))
	if err != nil {
		return nil, err
	}
	return r.Msg.GetReplicas(), nil
}
