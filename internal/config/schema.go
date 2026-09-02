package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

//go:generate go run ../../cmd/schema-gen -out ../../schema/orch.schema.json

const rawSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://change.codes/torsten/better-swarm/schema/orch.schema.json",
  "title": "orch application manifest",
  "description": "The strict v1alpha1 application contract for orch.",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema", "name", "services"],
  "properties": {
    "schema": {
      "const": "v1alpha1"
    },
    "name": {
      "$ref": "#/$defs/dnsLabel"
    },
    "services": {
      "type": "object",
      "minProperties": 1,
      "propertyNames": {
        "$ref": "#/$defs/dnsLabel"
      },
      "additionalProperties": {
        "$ref": "#/$defs/service"
      }
    },
    "volumes": {
      "type": "object",
      "default": {},
      "propertyNames": {
        "$ref": "#/$defs/dnsLabel"
      },
      "additionalProperties": {
        "$ref": "#/$defs/volume"
      }
    },
    "deploy": {
      "$ref": "#/$defs/deploy"
    }
  },
  "$defs": {
    "dnsLabel": {
      "type": "string",
      "pattern": "^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$",
      "maxLength": 63
    },
    "stringMap": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
    },
    "service": {
      "type": "object",
      "additionalProperties": false,
      "required": ["image"],
      "properties": {
        "image": {
          "type": "string",
          "minLength": 1
        },
        "command": {
          "type": "array",
          "items": {
            "type": "string",
            "minLength": 1
          }
        },
        "args": {
          "type": "array",
          "items": {
            "type": "string",
            "minLength": 1
          }
        },
        "replicas": {
          "type": "integer",
          "minimum": 1,
          "maximum": 100,
          "default": 1
        },
        "restart": {
          "type": "string",
          "enum": ["always", "on-failure", "never"],
          "default": "always"
        },
        "resources": {
          "$ref": "#/$defs/resources"
        },
        "http": {
          "$ref": "#/$defs/http"
        },
        "env": {
          "$ref": "#/$defs/envMap"
        },
        "secret_env": {
          "type": "object",
          "propertyNames": {
            "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"
          },
          "additionalProperties": {
            "$ref": "#/$defs/dnsLabel"
          }
        },
        "secret_files": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/secretFile"
          }
        },
        "constraints": {
          "$ref": "#/$defs/stringMap"
        },
        "mounts": {
          "type": "array",
          "items": {
            "$ref": "#/$defs/mount"
          }
        }
      }
    },
    "resources": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "cpu": {
          "type": "string",
          "pattern": "^[1-9][0-9]*(m)?$",
          "default": "100m"
        },
        "memory": {
          "type": "string",
          "pattern": "^[1-9][0-9]*(Ki|Mi|Gi|Ti)$",
          "default": "128Mi"
        }
      }
    },
    "envMap": {
      "type": "object",
      "propertyNames": {
        "pattern": "^[A-Za-z_][A-Za-z0-9_]*$"
      },
      "additionalProperties": {
        "type": "string"
      }
    },
    "http": {
      "type": "object",
      "additionalProperties": false,
      "required": ["port"],
      "properties": {
        "port": {
          "type": "integer",
          "minimum": 1,
          "maximum": 65535
        },
        "domains": {
          "type": "array",
          "uniqueItems": true,
          "items": {
            "type": "string",
            "format": "hostname"
          }
        },
        "health_path": {
          "type": "string",
          "pattern": "^/"
        },
        "https": {
          "type": "boolean",
          "default": true
        }
      },
      "allOf": [
        {
          "if": {
            "required": ["domains"],
            "properties": {
              "domains": {
                "minItems": 1
              }
            }
          },
          "then": {
            "required": ["health_path"]
          }
        }
      ]
    },
    "secretFile": {
      "type": "object",
      "additionalProperties": false,
      "required": ["source", "target"],
      "properties": {
        "source": {
          "$ref": "#/$defs/dnsLabel"
        },
        "target": {
          "type": "string",
          "pattern": "^/run/secrets/.+"
        }
      }
    },
    "mount": {
      "type": "object",
      "additionalProperties": false,
      "required": ["source", "target"],
      "properties": {
        "source": {
          "$ref": "#/$defs/dnsLabel"
        },
        "target": {
          "type": "string",
          "pattern": "^/.+"
        },
        "read_only": {
          "type": "boolean",
          "default": false
        }
      }
    },
    "volume": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "driver": {
          "const": "local",
          "default": "local"
        }
      }
    },
    "deploy": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "max_surge": {
          "type": "integer",
          "minimum": 0,
          "default": 1
        },
        "max_unavailable": {
          "type": "integer",
          "minimum": 0,
          "default": 0
        },
        "health_deadline": {
          "type": "string",
          "default": "2m"
        },
        "auto_rollback": {
          "type": "boolean",
          "default": true
        }
      }
    }
  }
}`

func SchemaJSON() ([]byte, error) {
	var output bytes.Buffer
	if err := json.Indent(&output, []byte(rawSchema), "", "  "); err != nil {
		return nil, fmt.Errorf("format embedded schema: %w", err)
	}
	output.WriteByte('\n')
	return output.Bytes(), nil
}
