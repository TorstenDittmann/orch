// Package buildinfo exposes the compatibility contract embedded in every orch binary.
package buildinfo

import "runtime"

var (
	Version = "0.1.0-dev"
	BuildID = "development"
)

const (
	InstallFormat        = 1
	ReleaseGeneration    = 1
	ControllerSchema     = 11
	AgentSchema          = 4
	AgentProtocol        = "v1alpha1-workloads-5"
	DependencyGeneration = "managed-bundle-1"
)

type Descriptor struct {
	Version              string `json:"version"`
	BuildID              string `json:"build_id"`
	OS                   string `json:"os"`
	Architecture         string `json:"architecture"`
	InstallFormat        int    `json:"install_format"`
	ReleaseGeneration    int    `json:"release_generation"`
	ControllerSchema     int    `json:"controller_schema"`
	AgentSchema          int    `json:"agent_schema"`
	AgentProtocol        string `json:"agent_protocol"`
	DependencyGeneration string `json:"dependency_generation"`
}

func Current() Descriptor {
	return Descriptor{Version: Version, BuildID: BuildID, OS: runtime.GOOS, Architecture: runtime.GOARCH,
		InstallFormat: InstallFormat, ReleaseGeneration: ReleaseGeneration, ControllerSchema: ControllerSchema, AgentSchema: AgentSchema,
		AgentProtocol: AgentProtocol, DependencyGeneration: DependencyGeneration}
}
