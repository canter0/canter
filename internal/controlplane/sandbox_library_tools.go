package controlplane

// sandboxLibraryTools shares the saved-environment API with both connected
// agents and Canter's conversation tools. These tools do not enable billing or
// products; mutations use the same authorization and budget checks as the UI.
func sandboxLibraryTools() []mcpTool {
	str := map[string]any{"type": "string"}
	version := map[string]any{"type": "integer", "minimum": 1, "maximum": 2147483647}
	tool := func(name, description string, properties map[string]any, required ...string) mcpTool {
		properties["workspaceId"] = str
		if required == nil {
			required = []string{}
		}
		return mcpTool{Name: name, Description: description, InputSchema: map[string]any{
			"type": "object", "properties": properties, "required": required, "additionalProperties": false,
		}}
	}
	return []mcpTool{
		tool("canter_sandbox_snapshots",
			"List this organization's available sandbox saves, newest first. Optional sourceSandboxId filters their origin; pass nextCursor back as before for another page. Sleep a sandbox to produce a save. Saves preserve files and supported package setup, not running processes or browser logins. historyEnabled reports whether billing permits extra kept saves and templates.",
			map[string]any{"sourceSandboxId": str, "before": str}),
		tool("canter_sandbox_snapshot_get",
			"Read metadata for a ready sandbox save in this organization. This does not download its archive or expose storage keys.",
			map[string]any{"id": str}, "id"),
		tool("canter_sandbox_snapshot_update",
			"Rename a sandbox save or change whether it is kept as extra history. Supply name, retained, or both. Keeping a new save requires active billing and uses storage pricing. Setting retained=false allows cleanup once no sandbox or template needs the save; it does not remove a current save.",
			map[string]any{"id": str, "name": str, "retained": map[string]any{"type": "boolean"}}, "id"),
		tool("canter_sandbox_templates",
			"List this organization's reusable sandbox templates and latest versions. Supply id to inspect a template's available versions. Use canter_sandbox_create with templateId and an explicit templateVersion to create an independent sandbox from a chosen version.",
			map[string]any{"id": str}),
		tool("canter_sandbox_template_create",
			"Create a named reusable template from a ready sandbox save. Requires active billing; referenced storage is counted once even when shared. Repeating the same name and source returns the existing template. This does not capture a running sandbox; sleep it first.",
			map[string]any{"name": str, "snapshotId": str}, "name", "snapshotId"),
		tool("canter_sandbox_template_version_add",
			"Add a ready save as a version of an existing sandbox template. Requires active billing. Adding the same save again returns its existing active version. Existing sandboxes keep their original save.",
			map[string]any{"id": str, "snapshotId": str}, "id", "snapshotId"),
		tool("canter_sandbox_template_delete",
			"Delete a sandbox template and all its versions. Existing sandboxes keep their saves. Saves not kept separately or referenced elsewhere become eligible for cleanup.",
			map[string]any{"id": str}, "id"),
		tool("canter_sandbox_template_version_delete",
			"Delete one version of a sandbox template. Existing sandboxes keep their saves. A template with no versions cannot create new sandboxes.",
			map[string]any{"id": str, "version": version}, "id", "version"),
		tool("canter_sandbox_restore",
			"Choose the save an asleep sandbox will restore on its next wake. Read its current generation with canter_sandbox_list and pass expectedGeneration; a stale generation is rejected. Requires active billing and a large enough disk. The previous save is kept as paid history. This does not wake the sandbox; use canter_sandbox_wake separately when intended.",
			map[string]any{"id": str, "snapshotId": str, "expectedGeneration": map[string]any{"type": "integer", "minimum": 1}}, "id", "snapshotId", "expectedGeneration"),
	}
}
