package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestWorkRequestSchemaPinsGoFields fails when a WorkRequest field is added to
// or dropped from one side only: the schema is additionalProperties:false, so
// a Go field the schema omits makes valid Go output fail schema validation.
func TestWorkRequestSchemaPinsGoFields(t *testing.T) {
	raw, err := os.ReadFile(repoPath(t, "protocol", "work-request-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var goFields []string
	typ := reflect.TypeOf(WorkRequest{})
	for i := 0; i < typ.NumField(); i++ {
		goFields = append(goFields, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	var schemaFields []string
	for name := range doc.Properties {
		schemaFields = append(schemaFields, name)
	}
	slices.Sort(goFields)
	slices.Sort(schemaFields)
	if !slices.Equal(goFields, schemaFields) {
		t.Fatalf("WorkRequest json fields %v differ from schema properties %v", goFields, schemaFields)
	}
}

func TestWorkRequestSchemaAcceptsResolver(t *testing.T) {
	schema := compileSchema(t, "work-request-v1.schema.json")
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"schema_version":"work-request/1","request_id":"req-12345678","identifiers":{"doi":"10.1000/x"},"resolver":"library"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("schema rejected resolver: %v", err)
	}
}
