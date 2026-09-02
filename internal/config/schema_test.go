package config

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestGeneratedSchemaIsCurrent(t *testing.T) {
	generated, err := SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(generated) {
		t.Fatal("SchemaJSON() returned invalid JSON")
	}
	checkedIn, err := os.ReadFile("../../schema/orch.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, checkedIn) {
		t.Fatal("schema/orch.schema.json is stale; run go generate ./internal/config")
	}
}
