package symphony

import "testing"

func TestRoleDescriptorsCoverEveryWorkRole(t *testing.T) {
	t.Parallel()
	roles := []WorkRole{RoleBaselineDraft, RoleBaselineVerification, RoleDiagnosis, RoleIteration, RoleIntegration, RoleFollowUp}
	for _, role := range roles {
		descriptor, ok := DescribeRole(role)
		if !ok || descriptor.Role != role || descriptor.ConfigurationKey == "" || descriptor.TerminalOperation == "" || len(descriptor.ToolCatalog) == 0 {
			t.Fatalf("incomplete descriptor for %s: %+v, %v", role, descriptor, ok)
		}
		if descriptor.FollowUpEligible {
			if name, ok := FollowUpInstructionName(role); !ok || name == "" {
				t.Fatalf("eligible role %s has no Follow-up instruction", role)
			}
		}
	}
	if _, ok := DescribeRole(WorkRole("unknown")); ok {
		t.Fatal("unknown role has a descriptor")
	}
}
