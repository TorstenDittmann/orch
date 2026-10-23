package images

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// RegistryHandler serves stored images read-only over the OCI distribution
// API, by digest only, so agents pull uploaded images like any registry
// image. The repository name in the path is ignored: content is addressed by
// digest and every stored image is served under any name.
func (s *Store) RegistryHandler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			response.Header().Set("Allow", "GET, HEAD")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		path := strings.TrimPrefix(request.URL.Path, "/v2/")
		if path == "" || path == request.URL.Path {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte("{}"))
			return
		}
		separator := strings.LastIndexByte(path, '/')
		if separator < 0 {
			registryError(response, http.StatusNotFound, "NAME_UNKNOWN")
			return
		}
		rest, value := path[:separator], path[separator+1:]
		parsed, err := digest.Parse(value)
		switch {
		case strings.HasSuffix(rest, "/manifests"):
			if err != nil {
				registryError(response, http.StatusNotFound, "MANIFEST_UNKNOWN")
				return
			}
			s.serveManifest(response, request, parsed)
		case strings.HasSuffix(rest, "/blobs"):
			if err != nil {
				registryError(response, http.StatusNotFound, "BLOB_UNKNOWN")
				return
			}
			s.serveBlob(response, request, parsed)
		default:
			registryError(response, http.StatusNotFound, "NAME_UNKNOWN")
		}
	})
}

func (s *Store) serveManifest(response http.ResponseWriter, request *http.Request, value digest.Digest) {
	data, err := s.readSmall(request.Context(), value)
	if err != nil {
		registryError(response, http.StatusNotFound, "MANIFEST_UNKNOWN")
		return
	}
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
	}
	if json.Unmarshal(data, &probe) != nil {
		registryError(response, http.StatusNotFound, "MANIFEST_UNKNOWN")
		return
	}
	mediaType := probe.MediaType
	if mediaType == "" {
		mediaType = ocispec.MediaTypeImageManifest
		if probe.Manifests != nil {
			mediaType = ocispec.MediaTypeImageIndex
		}
	}
	response.Header().Set("Content-Type", mediaType)
	response.Header().Set("Docker-Content-Digest", value.String())
	response.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if request.Method == http.MethodHead {
		response.WriteHeader(http.StatusOK)
		return
	}
	_, _ = response.Write(data)
}

func (s *Store) serveBlob(response http.ResponseWriter, request *http.Request, value digest.Digest) {
	ctx := request.Context()
	info, err := s.content.Info(ctx, value)
	if err != nil {
		registryError(response, http.StatusNotFound, "BLOB_UNKNOWN")
		return
	}
	reader, err := s.content.ReaderAt(ctx, ocispec.Descriptor{Digest: value, Size: info.Size})
	if err != nil {
		registryError(response, http.StatusNotFound, "BLOB_UNKNOWN")
		return
	}
	defer reader.Close()
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Docker-Content-Digest", value.String())
	response.Header().Set("Cache-Control", "max-age=31536000, immutable")
	http.ServeContent(response, request, "", time.Time{}, io.NewSectionReader(reader, 0, info.Size))
}

func registryError(response http.ResponseWriter, status int, code string) {
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": strings.ToLower(strings.ReplaceAll(code, "_", " "))}}})
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write(body.Bytes())
}
