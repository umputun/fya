// Package schemaoutput prepares JSON Schema structured-output validation.
package schemaoutput

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// NewValidator compiles schema and returns a hook that validates one JSON value.
func NewValidator(schema string) (func(string) (json.RawMessage, error), error) {
	compiled, err := compile(schema)
	if err != nil {
		return nil, err
	}
	return func(output string) (json.RawMessage, error) {
		raw := []byte(strings.TrimSpace(output))
		if len(raw) == 0 {
			return nil, errors.New("structured output is empty")
		}
		value, err := decodeJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("parse structured output JSON: %w", err)
		}
		if err := compiled.Validate(value); err != nil {
			return nil, fmt.Errorf("schema validation failed: %w", err)
		}
		return json.RawMessage(raw), nil
	}, nil
}

// Instruction returns prompt text that asks Claude for schema-valid JSON only.
func Instruction(schema string) string {
	return "\n\nReturn the final answer as exactly one JSON value that validates against this JSON Schema. " +
		"Do not include markdown fences, commentary, or any text outside the JSON value.\n\nJSON Schema:\n" + schema
}

func compile(schema string) (*jsonschema.Schema, error) {
	doc, err := decodeJSON([]byte(schema))
	if err != nil {
		return nil, fmt.Errorf("parse JSON schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	if addErr := compiler.AddResource("schema.json", doc); addErr != nil {
		return nil, fmt.Errorf("add JSON schema resource: %w", addErr)
	}
	compiled, compileErr := compiler.Compile("schema.json")
	if compileErr != nil {
		return nil, fmt.Errorf("compile JSON schema: %w", compileErr)
	}
	return compiled, nil
}

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON value: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode extra JSON value: %w", err)
		}
		return nil, errors.New("must be exactly one JSON value")
	}
	return value, nil
}
