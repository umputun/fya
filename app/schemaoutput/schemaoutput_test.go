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
	assert.NotContains(t, err.Error(), "validate structured output: validate structured output")
}

func TestValidatorPreservesLargeIntegerPrecision(t *testing.T) {
	validate, err := NewValidator(`{"const":9007199254740993}`)
	require.NoError(t, err)

	_, err = validate(`9007199254740992`)
	require.Error(t, err)

	raw, err := validate(`9007199254740993`)
	require.NoError(t, err)
	assert.Equal(t, `9007199254740993`, string(raw))
}

func TestValidatorSupportsDraft7Conditionals(t *testing.T) {
	schema := `{"if":{"properties":{"kind":{"const":"count"}},"required":["kind"]},` +
		`"then":{"required":["count"]},"else":{"required":["name"]}}`
	validate, err := NewValidator(schema)
	require.NoError(t, err)

	_, err = validate(`{"kind":"count","name":"missing count"}`)
	require.Error(t, err)

	raw, err := validate(`{"kind":"count","count":1}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"kind":"count","count":1}`, string(raw))
}

func TestValidatorRejectsEmptyOutput(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	_, err = validate("  \n\t ")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "structured output is empty")
}

func TestValidatorRejectsMultipleTopLevelValues(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	_, err = validate(`{"summary":"done"}{"summary":"again"}`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be exactly one JSON value")
}

func TestValidatorRejectsTrailingNonWhitespace(t *testing.T) {
	validate, err := NewValidator(objectSchema)
	require.NoError(t, err)

	_, err = validate(`{"summary":"done"} nope`)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse structured output JSON")
}

func TestValidatorAllowsSchemaValidNonObjectJSON(t *testing.T) {
	validate, err := NewValidator(`{"type":"string","enum":["ok"]}`)
	require.NoError(t, err)

	raw, err := validate(` "ok" `)

	require.NoError(t, err)
	assert.Equal(t, `"ok"`, string(raw))
}
