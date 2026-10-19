// Package pklschema exposes the versioned Pkl application schema embedded in
// the orch binary.
package pklschema

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"

	applepkl "github.com/apple/pkl-go/pkl"
)

const (
	// ApplicationURL is the schema URI manifests amend. Editors fetch it over
	// HTTPS; orch evaluation serves it from the embedded copy instead.
	ApplicationURL = SchemaBaseURL + "v1alpha1/Application.pkl"
	// SchemaBaseURL is the published directory that holds versioned schemas.
	SchemaBaseURL = "https://orch.torsten.work/pkl/"
	// LegacySchemaBaseURL is the earlier GitHub location, still served from
	// the embedded copy so existing manifests keep evaluating offline.
	LegacySchemaBaseURL = "https://raw.githubusercontent.com/TorstenDittmann/orch/main/pkl/"
	// ApplicationURI is the original orch: form, still accepted by orch.
	ApplicationURI     = "orch:/v1alpha1/Application.pkl"
	RequiredPklVersion = "0.32.1"
)

//go:embed v1alpha1/Application.pkl
var applicationSource string

func ApplicationSource() []byte {
	return []byte(applicationSource)
}

// ModuleReader exposes only the embedded, versioned application schema.
type ModuleReader struct{}

func (ModuleReader) Scheme() string {
	return "orch"
}

func (ModuleReader) IsGlobbable() bool {
	return false
}

func (ModuleReader) HasHierarchicalUris() bool {
	return true
}

func (ModuleReader) IsLocal() bool {
	return false
}

func (ModuleReader) ListElements(url.URL) ([]applepkl.PathElement, error) {
	return nil, fmt.Errorf("orch schema modules cannot be listed")
}

func (ModuleReader) Read(uri url.URL) (string, error) {
	if uri.String() != ApplicationURI {
		return "", fmt.Errorf("unknown orch schema module %q", uri.String())
	}
	return applicationSource, nil
}

// RunExternalReader serves the embedded schema over Pkl's external-reader
// protocol. It is plumbing for stock Pkl tooling, not application evaluation.
func RunExternalReader(requests io.Reader, responses io.Writer) error {
	client, err := applepkl.NewExternalReaderClient(
		applepkl.WithExternalClientStreams(requests, responses),
		applepkl.WithExternalClientModuleReader(ModuleReader{}),
	)
	if err != nil {
		return err
	}
	// Run owns protocol shutdown; CloseExternalProcess closes the client.
	return client.Run()
}

// Mirror serves the embedded schema on a loopback address so evaluation can
// rewrite ApplicationURL to it, staying offline and pinned to this binary.
type Mirror struct {
	server   *http.Server
	listener net.Listener
}

func StartMirror() (*Mirror, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start Pkl schema mirror: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1alpha1/Application.pkl", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(response, applicationSource)
	})
	mirror := &Mirror{server: &http.Server{Handler: mux}, listener: listener}
	go func() { _ = mirror.server.Serve(listener) }()
	return mirror, nil
}

// Rewrites maps the published schema directories to the mirror, in the form
// Pkl's HTTP rewrite settings expect.
func (m *Mirror) Rewrites() map[string]string {
	base := "http://" + m.listener.Addr().String() + "/"
	return map[string]string{SchemaBaseURL: base, LegacySchemaBaseURL: base}
}

// AllowedModule matches the rewritten schema URL, since Pkl checks the module
// allowlist after applying rewrites.
func (m *Mirror) AllowedModule() string {
	return `^http://` + regexp.QuoteMeta(m.listener.Addr().String()) + `/v1alpha1/Application\.pkl$`
}

func (m *Mirror) Close() error {
	return m.server.Close()
}
