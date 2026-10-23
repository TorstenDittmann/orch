package client

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
)

// uploadChunkBytes stays below the controller's per-message limit.
const uploadChunkBytes = 512 << 10

// LookupImage asks whether a local image needs an upload. An empty image
// skips the registry check.
func (client *Client) LookupImage(ctx context.Context, image, localID string) (*orchv1alpha1.LookupImageResponse, error) {
	response, err := client.service.LookupImage(ctx, connect.NewRequest(&orchv1alpha1.LookupImageRequest{Image: image, LocalId: localID}))
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}

// UploadImage streams an image archive written by docker image save to the
// controller and returns the digest it stored the image as.
func (client *Client) UploadImage(ctx context.Context, image, localID string, archive io.Reader) (*orchv1alpha1.UploadImageResponse, error) {
	uploads := client.uploads
	if uploads == nil {
		uploads = client.service
	}
	// Cancelling, rather than closing, the stream on a read error keeps the
	// controller from storing a truncated archive.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := uploads.UploadImage(ctx)
	buffer := make([]byte, uploadChunkBytes)
	message := &orchv1alpha1.UploadImageRequest{Image: image, LocalId: localID}
	for {
		n, readErr := io.ReadFull(archive, buffer)
		if n > 0 || message.GetImage() != "" {
			message.Data = buffer[:n]
			if err := stream.Send(message); err != nil {
				// The controller's reason arrives with the response.
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, err
			}
			message = &orchv1alpha1.UploadImageRequest{}
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			cancel()
			_, _ = stream.CloseAndReceive()
			return nil, readErr
		}
	}
	response, err := stream.CloseAndReceive()
	if err != nil {
		return nil, err
	}
	return response.Msg, nil
}
