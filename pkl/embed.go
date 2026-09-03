// Package pklschema exposes the versioned Pkl application schema embedded in
// the orch binary.
package pklschema

import (
	_ "embed"
	"fmt"
	"io"
	"net/url"

	applepkl "github.com/apple/pkl-go/pkl"
)

const (
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
