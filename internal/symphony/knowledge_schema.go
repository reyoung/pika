package symphony

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/knowledge-record-v1.schema.json
var knowledgeRecordSchemaFS embed.FS

var (
	compiledKnowledgeRecordSchema     *jsonschema.Schema
	compiledKnowledgeRecordSchemaErr  error
	compiledKnowledgeRecordSchemaOnce sync.Once
)

func KnowledgeRecordSchema() ([]byte, error) {
	contents, err := knowledgeRecordSchemaFS.ReadFile("schemas/knowledge-record-v1.schema.json")
	if err != nil {
		return nil, fmt.Errorf("read KnowledgeRecord v1 schema: %w", err)
	}
	return contents, nil
}

func ValidateKnowledgeRecordJSON(contents []byte) error {
	compiledKnowledgeRecordSchemaOnce.Do(func() {
		schemaBytes, err := KnowledgeRecordSchema()
		if err != nil {
			compiledKnowledgeRecordSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
		if err != nil {
			compiledKnowledgeRecordSchemaErr = fmt.Errorf("decode KnowledgeRecord v1 schema: %w", err)
			return
		}
		if err := compiler.AddResource("knowledge-record-v1.schema.json", value); err != nil {
			compiledKnowledgeRecordSchemaErr = err
			return
		}
		compiledKnowledgeRecordSchema, compiledKnowledgeRecordSchemaErr = compiler.Compile("knowledge-record-v1.schema.json")
	})
	if compiledKnowledgeRecordSchemaErr != nil {
		return compiledKnowledgeRecordSchemaErr
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("decode KnowledgeRecord v1: %w", err)
	}
	if err := compiledKnowledgeRecordSchema.Validate(value); err != nil {
		return fmt.Errorf("validate KnowledgeRecord v1: %w", err)
	}
	return nil
}

func validateKnowledgeRecord(record KnowledgeRecord) error {
	contents, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return ValidateKnowledgeRecordJSON(contents)
}
