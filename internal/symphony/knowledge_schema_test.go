package symphony

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/reyoung/pika-go/internal/kdacontract"
)

func TestKnowledgeRecordV1CanonicalFixtureAndStrictNegatives(t *testing.T) {
	t.Parallel()
	schema, err := KnowledgeRecordSchema()
	if err != nil || !bytes.Contains(schema, []byte(`knowledge-record-v1`)) {
		t.Fatalf("KnowledgeRecord schema = %q, err=%v", schema, err)
	}
	valid := []byte(`{
		"schema_version":1,
		"knowledge_id":"experiment:exp-1",
		"kind":"experiment-result",
		"status":"provisional",
		"scope":{
			"case_ids":["case-a"],
			"best_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"baseline_revision_id":"baseline-1",
			"hardware":{"gpu":"h100"},
			"software":{"driver":"550"}
		},
		"summary":"measured improvement",
		"source":{"diagnosis_id":"diagnosis-1","experiment_id":"exp-1"},
		"evidence_artifact_ids":["artifact-1"],
		"experiment":{"derived_comparisons":[],"artifact_ids":["artifact-1"]}
	}`)
	if err := ValidateKnowledgeRecordJSON(valid); err != nil {
		t.Fatalf("canonical KnowledgeRecord fixture: %v", err)
	}
	type patch func([]byte) []byte
	replace := func(old, next string) patch {
		return func(in []byte) []byte { return bytes.Replace(in, []byte(old), []byte(next), 1) }
	}
	removeScopeField := func(field string) patch {
		return func(in []byte) []byte {
			var record map[string]any
			if err := json.Unmarshal(in, &record); err != nil {
				t.Fatal(err)
			}
			scope := record["scope"].(map[string]any)
			delete(scope, field)
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		}
	}
	for name, mutate := range map[string]patch{
		"unknown field":       replace(`"summary":"measured improvement"`, `"summary":"measured improvement","raw_payload":"forbidden"`),
		"missing cases":       replace(`"case_ids":["case-a"]`, `"case_ids":[]`),
		"missing baseline":    removeScopeField("baseline_revision_id"),
		"unknown status":      replace(`"status":"provisional"`, `"status":"trusted"`),
		"empty source":        replace(`"source":{"diagnosis_id":"diagnosis-1","experiment_id":"exp-1"}`, `"source":{}`),
		"duplicate artifacts": replace(`"evidence_artifact_ids":["artifact-1"]`, `"evidence_artifact_ids":["artifact-1","artifact-1"]`),
		"missing hardware":    removeScopeField("hardware"),
		"missing software":    removeScopeField("software"),
		"nested hardware":     replace(`"hardware":{"gpu":"h100"}`, `"hardware":{"gpu":{"nested":true}}`),
	} {
		invalid := mutate(valid)
		if err := ValidateKnowledgeRecordJSON(invalid); err == nil {
			t.Fatalf("accepted invalid KnowledgeRecord fixture %q: %s", name, invalid)
		}
	}
	emptyEnv := bytes.Replace(valid, []byte(`"hardware":{"gpu":"h100"}`), []byte(`"hardware":{}`), 1)
	emptyEnv = bytes.Replace(emptyEnv, []byte(`"software":{"driver":"550"}`), []byte(`"software":{}`), 1)
	if err := ValidateKnowledgeRecordJSON(emptyEnv); err != nil {
		t.Fatalf("empty environment objects must remain valid: %v", err)
	}
}

func TestKnowledgeRecordEnvironmentProfileMatchesDiagnosis(t *testing.T) {
	t.Parallel()
	var want any
	encoded, err := json.Marshal(kdacontract.EnvironmentProfileSchema())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &want); err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	contents, err := KnowledgeRecordSchema()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &schema); err != nil {
		t.Fatal(err)
	}
	defs, _ := schema["$defs"].(map[string]any)
	if !reflect.DeepEqual(defs["environmentProfile"], want) {
		t.Fatalf("Knowledge environment profile = %#v, Diagnosis = %#v", defs["environmentProfile"], want)
	}
}
