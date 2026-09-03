package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ParseCanonicalJSON validates the controller-side wire representation without
// invoking Pkl or applying authoring defaults.
func ParseCanonicalJSON(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return nil, errors.New("canonical manifest JSON is required")
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("canonical manifest exceeds %d bytes", MaxManifestBytes)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode canonical manifest JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := Validate(&manifest); err != nil {
		return nil, err
	}
	normalize(&manifest)
	canonical, err := CanonicalJSON(&manifest)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("manifest JSON is not in canonical form; use `orch render`")
	}
	return &manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode canonical manifest JSON: %w", err)
	}
	return errors.New("decode canonical manifest JSON: trailing JSON value")
}
