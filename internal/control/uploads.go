package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"
	orchv1alpha1 "github.com/TorstenDittmann/orch/gen/orch/v1alpha1"
	"github.com/TorstenDittmann/orch/internal/images"
	"github.com/distribution/reference"
)

// uploadRetention is how many recent revisions of each application keep
// their uploaded images, so rollbacks to them still work.
const uploadRetention = 10

const uploadCollectInterval = time.Hour

var imageDigestPattern = regexp.MustCompile(`sha256:[a-f0-9]{64}`)

// LookupImage tells orch deploy whether an image it found in the local engine
// needs an upload: not when the image's registry has it, and not when the
// same local image was uploaded before.
func (s *service) LookupImage(ctx context.Context, request *connect.Request[orchv1alpha1.LookupImageRequest]) (*connect.Response[orchv1alpha1.LookupImageResponse], error) {
	owner, err := s.requireUploads(ctx)
	if err != nil {
		return nil, err
	}
	if !images.ValidLocalID(request.Msg.GetLocalId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("local_id must be an image ID"))
	}
	if image := request.Msg.GetImage(); image != "" {
		resolveContext, cancel := context.WithTimeout(ctx, time.Minute)
		_, err := s.resolveImage(resolveContext, "image", image)
		cancel()
		if err == nil {
			return connect.NewResponse(&orchv1alpha1.LookupImageResponse{InRegistry: true}), nil
		}
		// Only an image the registry lacks is uploaded. Docker Hub answers
		// unauthorized for repositories that do not exist. Any other error,
		// such as a rate limit or outage, must not silently swap a registry
		// image for a local copy.
		if !errors.Is(err, images.ErrNotFound) && !errors.Is(err, images.ErrUnauthorized) {
			return nil, err
		}
	}
	digest, err := s.uploads.Lookup(ctx, owner, request.Msg.GetLocalId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("read the image store"))
	}
	return connect.NewResponse(&orchv1alpha1.LookupImageResponse{Digest: digest}), nil
}

func (s *service) UploadImage(ctx context.Context, stream *connect.ClientStream[orchv1alpha1.UploadImageRequest]) (*connect.Response[orchv1alpha1.UploadImageResponse], error) {
	owner, err := s.requireUploads(ctx)
	if err != nil {
		return nil, err
	}
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the upload is empty"))
	}
	first := stream.Msg()
	if _, err := reference.ParseNormalizedNamed(first.GetImage()); err != nil || len(first.GetImage()) > 1024 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image must be an image reference"))
	}
	if !images.ValidLocalID(first.GetLocalId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("local_id must be an image ID"))
	}
	reader := &uploadReader{stream: stream, pending: first.GetData()}
	digest, platforms, err := s.uploads.Import(ctx, owner, first.GetImage(), first.GetLocalId(), reader)
	switch {
	case reader.err != nil:
		return nil, reader.err
	case errors.Is(err, context.Canceled):
		return nil, connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, images.ErrNoSpace):
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, images.ErrPlatform):
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: %w", first.GetImage(), images.ErrPlatform))
	case errors.Is(err, images.ErrArchive), errors.Is(err, images.ErrInvalid):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case err != nil:
		log.Printf("controller: store uploaded image %s: %v", first.GetImage(), err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("store the uploaded image"))
	}
	return connect.NewResponse(&orchv1alpha1.UploadImageResponse{Digest: digest, Platforms: platforms}), nil
}

// requireUploads admits anyone who may deploy and returns who owns their
// uploads: the application a deployer is limited to, or "" for
// administrators. Uploads are content-addressed, and lookups only find the
// caller's own uploads, so a deployer cannot change another application's
// images.
func (s *service) requireUploads(ctx context.Context) (string, error) {
	owner, err := requireDeployerOrAdministrator(ctx)
	if err != nil {
		return "", err
	}
	if err := requireChange(ctx); err != nil {
		return "", err
	}
	if s.uploads == nil {
		return "", connect.NewError(connect.CodeUnimplemented, errors.New("this controller does not store images"))
	}
	return owner, nil
}

// uploadIdleTimeout ends an upload that stopped sending, so it cannot hold
// the image store forever.
const uploadIdleTimeout = 2 * time.Minute

// idleUploads moves the request's read deadline forward on every read of
// the upload body.
func idleUploads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		controller := http.NewResponseController(response)
		request.Body = &deadlineBody{ReadCloser: request.Body, extend: func() {
			_ = controller.SetReadDeadline(time.Now().Add(uploadIdleTimeout))
		}}
		next.ServeHTTP(response, request)
	})
}

type deadlineBody struct {
	io.ReadCloser
	extend func()
}

func (b *deadlineBody) Read(buffer []byte) (int, error) {
	b.extend()
	return b.ReadCloser.Read(buffer)
}

// uploadReader turns the chunks of an upload stream into the image archive.
type uploadReader struct {
	stream  *connect.ClientStream[orchv1alpha1.UploadImageRequest]
	pending []byte
	total   int64
	err     error
}

func (r *uploadReader) Read(buffer []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				r.err = err
				return 0, err
			}
			return 0, io.EOF
		}
		message := r.stream.Msg()
		if message.GetImage() != "" || message.GetLocalId() != "" {
			r.err = connect.NewError(connect.CodeInvalidArgument, errors.New("only the first upload message may set image and local_id"))
			return 0, r.err
		}
		r.pending = message.GetData()
		r.total += int64(len(r.pending))
		if r.total > images.MaxUploadBytes {
			r.err = connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the image is larger than %d GiB", images.MaxUploadBytes>>30))
			return 0, r.err
		}
	}
	n := copy(buffer, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// runUploadCollection removes uploaded images that no recent deployment uses.
func (s *service) runUploadCollection(ctx context.Context) {
	if s.uploads == nil {
		return
	}
	ticker := time.NewTicker(uploadCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.collectUploads(ctx); err != nil && ctx.Err() == nil {
				log.Printf("controller: collect uploaded images: %v", err)
			}
		}
	}
}

func (s *service) collectUploads(ctx context.Context) error {
	manifests, err := s.state.RecentManifests(ctx, uploadRetention)
	if err != nil {
		return fmt.Errorf("list recent deployments: %w", err)
	}
	keep := map[string]bool{}
	for _, manifest := range manifests {
		for _, digest := range imageDigestPattern.FindAll(manifest, -1) {
			keep[string(digest)] = true
		}
	}
	return s.uploads.Collect(ctx, keep)
}
