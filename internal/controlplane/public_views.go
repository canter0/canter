package controlplane

import "github.com/canter0/canter/sdk"

const redactedValue = "[redacted]"

func publicChange(change sdk.Change) sdk.Change {
	change.Plan.Release.ArtifactKey = ""
	change.Plan.Release.Command = publicCommand(change.Plan.Release.Command)
	change.Plan.Release.Environment = redactedEnvironment(change.Plan.Release.Environment)
	if change.Plan.Migration != nil {
		migration := *change.Plan.Migration
		migration.SQL = ""
		change.Plan.Migration = &migration
	}
	change.Operations = append([]sdk.ChangeOperation(nil), change.Operations...)
	for index := range change.Operations {
		if change.Operations[index].Failure != "" {
			change.Operations[index].Failure = "operation failed; operator inspection required"
		}
	}
	if change.Failure != "" {
		change.Failure = "Change failed; operator inspection required"
	}
	if len(change.Residuals) > 0 {
		change.Residuals = []string{"residual state requires operator inspection"}
	}
	return change
}

func publicChangeInspection(inspection ChangeInspection) ChangeInspection {
	inspection.Change = publicChange(inspection.Change)
	return inspection
}

func publicPolicyApplyResult(result PolicyApplyResult) PolicyApplyResult {
	result.Change = publicChange(result.Change)
	if result.Decision.Failure != "" {
		result.Decision.Failure = "policy application failed; operator inspection required"
	}
	return result
}

func publicInitialDeployment(deployment InitialDeployment) InitialDeployment {
	deployment.Plan.Release.Command = publicCommand(deployment.Plan.Release.Command)
	deployment.Plan.Release.Environment = redactedEnvironment(deployment.Plan.Release.Environment)
	deployment.Operations = append([]InitialDeploymentOperation(nil), deployment.Operations...)
	for index := range deployment.Operations {
		if deployment.Operations[index].Failure != "" {
			deployment.Operations[index].Failure = "operation failed; operator inspection required"
		}
	}
	if deployment.Failure != "" {
		deployment.Failure = "initial deployment failed; operator inspection required"
	}
	return deployment
}

func redactedEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return nil
	}
	redacted := make(map[string]string, len(environment))
	for key := range environment {
		redacted[key] = redactedValue
	}
	return redacted
}

func publicCommand(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	public := make([]string, len(command))
	public[0] = command[0]
	for index := 1; index < len(command); index++ {
		public[index] = "[redacted-argument]"
	}
	return public
}
