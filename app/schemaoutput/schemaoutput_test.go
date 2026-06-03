package schemaoutput

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const objectSchema = `{"type":"object","required":["summary"],` +
	`"properties":{"summary":{"type":"string"}},"additionalProperties":false}`

func TestNewValidatorValidSchema(t *testing.T) {
	validate, err := NewValidator(objectSchema)

	require.NoError(t, err)
	assert.NotNil(t, validate)
}

func TestValidatorAcceptsValidInstance(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	raw, err := validate(` {"summary":"done"} `)

	require.NoError(t, err)
	assert.JSONEq(t, `{"summary":"done"}`, string(raw))
}

func TestInstructionIncludesSchema(t *testing.T) {
	got := Instruction(objectSchema)

	assert.Contains(t, got, "exactly one JSON value")
	assert.Contains(t, got, "Do not include markdown fences")
	assert.Contains(t, got, "JSON Schema:\n"+objectSchema)
}

func TestNewValidatorRejectsInvalidSchema(t *testing.T) {
	_, err := NewValidator(`{"type":"not-a-json-schema-type"}`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile JSON schema")
}

func TestValidatorRejectsInvalidJSONOutput(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	_, err = validate(`not json`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse structured output JSON")
}

func TestValidatorRejectsSchemaMismatch(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	_, err = validate(`{"summary":7}`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate structured output")
}

func TestValidatorAllowsSchemaValidNonObjectJSON(t *testing.T) {
	validate, err := NewValidator(`{"type":"string","enum":["ok"]}`)
	require.NoError(t, err)

	raw, err := validate(` "ok" `)

	require.NoError(t, err)
	assert.Equal(t, `"ok"`, string(raw))
}
