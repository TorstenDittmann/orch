package model

import "time"

const (
	EventSecretVersionCreated      = "secret.version.created"
	EventSecretDeleted             = "secret.deleted"
	EventRegistryCredentialUpdated = "registry.credential.updated"
	EventRegistryCredentialDeleted = "registry.credential.deleted"
)

type SecretMetadata struct {
	Application      string    `json:"application"`
	Name             string    `json:"name"`
	CurrentVersionID string    `json:"current_version_id"`
	Referenced       bool      `json:"referenced"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type RegistryCredentialMetadata struct {
	Host             string    `json:"host"`
	Username         string    `json:"username"`
	CurrentVersionID string    `json:"current_version_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type SecretMutationResult struct {
	Application   string    `json:"application,omitempty"`
	Name          string    `json:"name,omitempty"`
	Host          string    `json:"host,omitempty"`
	Username      string    `json:"username,omitempty"`
	VersionID     string    `json:"version_id,omitempty"`
	Changed       bool      `json:"changed"`
	EventSequence uint64    `json:"event_sequence"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type ResolvedSecret struct {
	Application string
	Name        string
	VersionID   string
	Value       []byte
}

type ResolvedRegistryCredential struct {
	Host      string
	Username  string
	VersionID string
	Secret    []byte
}
