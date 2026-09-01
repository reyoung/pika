package symphony

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/reyoung/pika-go/internal/kdacontract"
)

type KnowledgeScope struct {
	CaseIDs            []string       `json:"case_ids"`
	BestSHA            string         `json:"best_sha"`
	BaselineRevisionID string         `json:"baseline_revision_id,omitempty"`
	Hardware           map[string]any `json:"hardware"`
	Software           map[string]any `json:"software"`
}

type KnowledgeSource struct {
	DiagnosisID   string `json:"diagnosis_id,omitempty"`
	ExperimentID  string `json:"experiment_id,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
}

type ExperimentKnowledgeEvidence struct {
	DerivedComparisons []BenchmarkComparisonView `json:"derived_comparisons,omitempty"`
	ArtifactIDs        []string                  `json:"artifact_ids,omitempty"`
}

type KnowledgeRecord struct {
	SchemaVersion       int64                        `json:"schema_version"`
	KnowledgeID         string                       `json:"knowledge_id"`
	Kind                string                       `json:"kind"`
	Status              string                       `json:"status"`
	Scope               KnowledgeScope               `json:"scope"`
	Summary             string                       `json:"summary"`
	HypothesisMechanism string                       `json:"hypothesis_mechanism,omitempty"`
	Source              KnowledgeSource              `json:"source"`
	EvidenceArtifactIDs []string                     `json:"evidence_artifact_ids,omitempty"`
	Experiment          *ExperimentKnowledgeEvidence `json:"experiment,omitempty"`
}

func BuildKnowledgeProjection(work RuntimeWork, view View, experiments []IterationExperimentView, artifacts []EvidenceArtifact) ([]KnowledgeRecord, error) {
	type experimentRecord struct {
		Summary    string `json:"summary"`
		Hypothesis struct {
			DiagnosisHypothesisID string `json:"diagnosis_hypothesis_id"`
		} `json:"hypothesis"`
	}

	records := make([]KnowledgeRecord, 0, len(experiments)+16)
	recordIndexByID := map[string]int{}
	observationByID := map[string]kdacontract.DiagnosisObservation{}
	bottleneckByID := map[string]kdacontract.DiagnosisBottleneck{}
	hypothesisByID := map[string]kdacontract.DiagnosisHypothesis{}
	var diagnosisScope KnowledgeScope

	appendRecord := func(record KnowledgeRecord) {
		recordIndexByID[record.KnowledgeID] = len(records)
		records = append(records, record)
	}
	promote := func(id, status, integrationID string) {
		index, exists := recordIndexByID[id]
		if !exists {
			return
		}
		records[index].Status = status
		records[index].Source.IntegrationID = integrationID
	}

	artifactIDsByWorkAndPath := map[string][]string{}
	for _, artifact := range artifacts {
		key := artifact.WorkID + "\x00" + artifact.RelativePath
		artifactIDsByWorkAndPath[key] = append(artifactIDsByWorkAndPath[key], artifact.ID)
	}
	for key := range artifactIDsByWorkAndPath {
		sort.Strings(artifactIDsByWorkAndPath[key])
	}

	if work.Diagnosis != nil && len(work.Diagnosis.Report) != 0 {
		report, err := kdacontract.ParseDiagnosisReport(work.Diagnosis.Report)
		if err != nil {
			return nil, err
		}
		scopeTemplate := KnowledgeScope{
			BestSHA:            report.Subject.BestSHA,
			BaselineRevisionID: report.Subject.BaselineRevisionID,
			Hardware:           report.Subject.Hardware,
			Software:           report.Subject.Software,
		}
		diagnosisScope = scopeTemplate
		diagnosisScope.CaseIDs = append([]string{}, report.Coverage.CaseIDs...)
		for _, observation := range report.Observations {
			observationByID[observation.ID] = observation
			artifactIDs := make([]string, 0, len(observation.SourceArtifacts))
			for _, relativePath := range observation.SourceArtifacts {
				key := work.Diagnosis.WorkID + "\x00" + relativePath
				artifactIDs = append(artifactIDs, artifactIDsByWorkAndPath[key]...)
			}
			record := KnowledgeRecord{
				SchemaVersion: 1, KnowledgeID: "diagnosis:" + work.Diagnosis.ID + ":observation:" + observation.ID,
				Kind: "observation", Status: "provisional", Scope: scopeTemplate, Summary: observation.Summary,
				Source: KnowledgeSource{DiagnosisID: work.Diagnosis.ID}, EvidenceArtifactIDs: artifactIDs,
			}
			record.Scope.CaseIDs = append([]string{}, observation.CaseIDs...)
			appendRecord(record)
		}
		for _, bottleneck := range report.Bottlenecks {
			bottleneckByID[bottleneck.ID] = bottleneck
			record := KnowledgeRecord{
				SchemaVersion: 1, KnowledgeID: "diagnosis:" + work.Diagnosis.ID + ":bottleneck:" + bottleneck.ID,
				Kind: "bottleneck", Status: "provisional", Scope: scopeTemplate, Summary: bottleneck.Summary,
				Source: KnowledgeSource{DiagnosisID: work.Diagnosis.ID},
			}
			for _, observationID := range bottleneck.ObservationIDs {
				record.Scope.CaseIDs = append(record.Scope.CaseIDs, observationByID[observationID].CaseIDs...)
			}
			record.Scope.CaseIDs = uniqueSortedStrings(record.Scope.CaseIDs)
			appendRecord(record)
		}
		for _, hypothesis := range report.Hypotheses {
			hypothesisByID[hypothesis.ID] = hypothesis
			record := KnowledgeRecord{
				SchemaVersion: 1, KnowledgeID: "diagnosis:" + work.Diagnosis.ID + ":hypothesis:" + hypothesis.ID,
				Kind: "hypothesis", Status: "provisional", Scope: scopeTemplate, Summary: hypothesis.Summary,
				HypothesisMechanism: hypothesis.Mechanism, Source: KnowledgeSource{DiagnosisID: work.Diagnosis.ID},
			}
			record.Scope.CaseIDs = append([]string{}, hypothesis.TargetCaseIDs...)
			appendRecord(record)
		}
	}

	experimentKnowledgeID := map[string]string{}
	experimentHypothesisRef := map[string]string{}
	for _, experiment := range experiments {
		status := "provisional"
		if experiment.Outcome == "rejected" {
			status = "observed-negative"
		} else if experiment.Outcome == "inconclusive" {
			status = "inconclusive"
		}
		summary := experiment.Outcome + " experiment"
		var payload experimentRecord
		if len(experiment.Experiment) != 0 && json.Unmarshal(experiment.Experiment, &payload) == nil {
			if payload.Summary != "" {
				summary = payload.Summary
			}
			experimentHypothesisRef[experiment.ID] = payload.Hypothesis.DiagnosisHypothesisID
		}
		if experiment.ScopeBestSHA == "" {
			return nil, fmt.Errorf("Experiment %s is missing persisted scope_best_sha", experiment.ID)
		}
		if diagnosisScope.BaselineRevisionID == "" {
			return nil, fmt.Errorf("Experiment %s has no frozen Diagnosis scope", experiment.ID)
		}
		scope := diagnosisScope
		scope.BestSHA = experiment.ScopeBestSHA
		scope.CaseIDs = nil
		for _, comparison := range experiment.DerivedComparisons {
			if comparison.CaseID != "" {
				scope.CaseIDs = append(scope.CaseIDs, comparison.CaseID)
			}
		}
		if len(scope.CaseIDs) == 0 {
			scope.CaseIDs = append(scope.CaseIDs, hypothesisByID[payload.Hypothesis.DiagnosisHypothesisID].TargetCaseIDs...)
		}
		if len(scope.CaseIDs) == 0 {
			scope.CaseIDs = append(scope.CaseIDs, diagnosisScope.CaseIDs...)
		}
		scope.CaseIDs = uniqueSortedStrings(scope.CaseIDs)
		source := KnowledgeSource{ExperimentID: experiment.ID}
		if payload.Hypothesis.DiagnosisHypothesisID != "" && work.Diagnosis != nil {
			source.DiagnosisID = work.Diagnosis.ID
		}
		record := KnowledgeRecord{
			SchemaVersion: 1, KnowledgeID: "experiment:" + experiment.ID, Kind: "experiment-result", Status: status,
			Scope: scope, Summary: summary, Source: source,
			Experiment: &ExperimentKnowledgeEvidence{
				DerivedComparisons: append([]BenchmarkComparisonView{}, experiment.DerivedComparisons...),
				ArtifactIDs:        append([]string{}, experiment.ArtifactIDs...),
			},
		}
		appendRecord(record)
		experimentKnowledgeID[experiment.ID] = record.KnowledgeID
	}

	for _, integration := range view.Integrations {
		if integration.CandidateExperimentID == "" {
			continue
		}
		experimentID := integration.CandidateExperimentID
		experimentRecordID, exists := experimentKnowledgeID[experimentID]
		if !exists {
			continue
		}
		switch integration.Status {
		case "accepted":
			promote(experimentRecordID, "verified", integration.ID)
			if work.Diagnosis == nil {
				continue
			}
			hypothesisID := experimentHypothesisRef[experimentID]
			if hypothesisID == "" {
				continue
			}
			promote("diagnosis:"+work.Diagnosis.ID+":hypothesis:"+hypothesisID, "verified", integration.ID)
			hypothesis := hypothesisByID[hypothesisID]
			for _, bottleneckID := range hypothesis.BottleneckIDs {
				promote("diagnosis:"+work.Diagnosis.ID+":bottleneck:"+bottleneckID, "verified", integration.ID)
				for _, observationID := range bottleneckByID[bottleneckID].ObservationIDs {
					observation := observationByID[observationID]
					promote("diagnosis:"+work.Diagnosis.ID+":observation:"+observation.ID, "verified", integration.ID)
				}
			}
		case "rejected":
			promote(experimentRecordID, "integration-rejected", integration.ID)
		}
	}

	for _, record := range records {
		if err := validateKnowledgeRecord(record); err != nil {
			return nil, fmt.Errorf("generated KnowledgeRecord %s is invalid: %w", record.KnowledgeID, err)
		}
	}
	return records, nil
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
