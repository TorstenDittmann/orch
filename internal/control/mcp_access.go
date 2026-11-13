package control

import (
	"github.com/TorstenDittmann/orch/gen/orch/v1alpha1/orchv1alpha1connect"
	"github.com/TorstenDittmann/orch/internal/model"
)

// mcpReadProcedures are what the read-only MCP tools call.
var mcpReadProcedures = []string{
	orchv1alpha1connect.ControlServiceGetClusterProcedure,
	orchv1alpha1connect.ControlServiceListNodesProcedure,
	orchv1alpha1connect.ControlServiceGetNodeUsageProcedure,
	orchv1alpha1connect.ControlServiceListApplicationsProcedure,
	orchv1alpha1connect.ControlServiceGetApplicationProcedure,
	orchv1alpha1connect.ControlServiceListRuntimesProcedure,
	orchv1alpha1connect.ControlServiceListDeploymentsProcedure,
	orchv1alpha1connect.ControlServiceGetDeploymentProcedure,
	orchv1alpha1connect.ControlServiceCompareDeploymentsProcedure,
	orchv1alpha1connect.ControlServiceListEventsProcedure,
	orchv1alpha1connect.ControlServiceListRecentEventsProcedure,
	orchv1alpha1connect.ControlServiceTailLogsProcedure,
	orchv1alpha1connect.ControlServiceGetJobProcedure,
	orchv1alpha1connect.ControlServiceListJobsProcedure,
	orchv1alpha1connect.ControlServiceListCronRunsProcedure,
	orchv1alpha1connect.ControlServiceTailCronLogsProcedure,
	orchv1alpha1connect.ControlServiceListSecretsProcedure,
	orchv1alpha1connect.ControlServiceListRegistriesProcedure,
	orchv1alpha1connect.ControlServiceListVolumesProcedure,
}

// mcpDeployProcedures change workloads without deleting or replacing them.
var mcpDeployProcedures = []string{
	orchv1alpha1connect.ControlServiceRollbackApplicationProcedure,
	orchv1alpha1connect.ControlServiceRestartServiceProcedure,
	orchv1alpha1connect.ControlServiceRetryJobProcedure,
	orchv1alpha1connect.ControlServiceRunCronJobProcedure,
	orchv1alpha1connect.ControlServiceBackupVolumeProcedure,
	orchv1alpha1connect.ControlServiceLoginRegistryProcedure,
}

// mcpDestructiveProcedures are what the destructive MCP tools call. Cluster
// administration that no MCP tool needs, such as tokens, trust, join tokens
// or node removal, stays out of reach at every level.
var mcpDestructiveProcedures = []string{
	orchv1alpha1connect.ControlServiceRemoveApplicationProcedure,
	orchv1alpha1connect.ControlServiceExecProcedure,
	orchv1alpha1connect.ControlServiceRunJobProcedure,
	orchv1alpha1connect.ControlServiceCancelJobProcedure,
	orchv1alpha1connect.ControlServiceSetSecretProcedure,
	orchv1alpha1connect.ControlServiceDeleteSecretProcedure,
	orchv1alpha1connect.ControlServiceLogoutRegistryProcedure,
	orchv1alpha1connect.ControlServiceRestoreVolumeProcedure,
	orchv1alpha1connect.ControlServiceMoveVolumeProcedure,
	orchv1alpha1connect.ControlServiceUpdateNodeProcedure,
}

// mcpProcedures returns the control API procedures an MCP access level may
// call. Unknown levels get nothing.
func mcpProcedures(access string) map[string]bool {
	var lists [][]string
	switch access {
	case model.MCPAccessRead:
		lists = [][]string{mcpReadProcedures}
	case model.MCPAccessDeploy:
		lists = [][]string{mcpReadProcedures, mcpDeployProcedures}
	case model.MCPAccessDestructive:
		lists = [][]string{mcpReadProcedures, mcpDeployProcedures, mcpDestructiveProcedures}
	}
	allowed := map[string]bool{}
	for _, list := range lists {
		for _, procedure := range list {
			allowed[procedure] = true
		}
	}
	return allowed
}
