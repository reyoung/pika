package symphony

type RoleDescriptor struct {
	Role              WorkRole
	ConfigurationKey  string
	InstructionName   string
	TerminalOperation string
	ToolCatalog       []string
	InjectSkills      bool
	FollowUpEligible  bool
}

var roleDescriptors = map[WorkRole]RoleDescriptor{
	RoleBaselineDraft: {
		Role: RoleBaselineDraft, ConfigurationKey: "baseline", InstructionName: "baseline",
		TerminalOperation: "submit_baseline_definition", ToolCatalog: []string{"commit_changes", "submit_baseline_definition"}, InjectSkills: true,
	},
	RoleBaselineVerification: {
		Role: RoleBaselineVerification, ConfigurationKey: "baseline_verify", InstructionName: "baseline-verify",
		TerminalOperation: "finish_baseline_verification", ToolCatalog: []string{"finish_baseline_verification"}, InjectSkills: true, FollowUpEligible: true,
	},
	RoleDiagnosis: {
		Role: RoleDiagnosis, ConfigurationKey: "iteration", InstructionName: "diagnosis",
		TerminalOperation: "finish_diagnosis", ToolCatalog: []string{"finish_diagnosis"}, InjectSkills: true, FollowUpEligible: true,
	},
	RoleIteration: {
		Role: RoleIteration, ConfigurationKey: "iteration", InstructionName: "iteration",
		TerminalOperation: "finish_iteration", ToolCatalog: []string{"commit_changes", "record_iteration_experiment", "finish_iteration"}, InjectSkills: true, FollowUpEligible: true,
	},
	RoleIntegration: {
		Role: RoleIntegration, ConfigurationKey: "integration", InstructionName: "integration",
		TerminalOperation: "finish_integration", ToolCatalog: []string{"prepare_best_update", "apply_best_update", "finish_integration"}, InjectSkills: true, FollowUpEligible: true,
	},
	RoleFollowUp: {
		Role: RoleFollowUp, ConfigurationKey: "follow_up",
		TerminalOperation: "submit_followup_message", ToolCatalog: []string{"submit_followup_message"},
	},
}

func DescribeRole(role WorkRole) (RoleDescriptor, bool) {
	descriptor, ok := roleDescriptors[role]
	descriptor.ToolCatalog = append([]string{}, descriptor.ToolCatalog...)
	return descriptor, ok
}

func FollowUpInstructionName(role WorkRole) (string, bool) {
	descriptor, ok := DescribeRole(role)
	if !ok || !descriptor.FollowUpEligible {
		return "", false
	}
	return "follow-up/" + descriptor.InstructionName, true
}
