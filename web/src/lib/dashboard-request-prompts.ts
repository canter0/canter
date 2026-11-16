import type { Sandbox } from "./sandboxes";

export type SandboxRequestAction = "inspect" | "sleep" | "wake" | "delete" | "command" | "preview" | "settings" | "save" | "desktop";

export const dashboardRequestPrompts = {
  createSandbox: "Help me create a sandbox in this organisation. Confirm the configuration, cost and access before creating it.",
  savedEnvironments: "Help me manage this organisation's sandbox saves and templates. Inspect the available saves before proposing changes.",
  deployApp: "Help me deploy an application in this organisation. Prepare the deployment for the required policy checks and human review.",
};

export function sandboxRequestPrompt(action: SandboxRequestAction, sandbox: Pick<Sandbox, "id" | "name">): string {
  const target = `sandbox ${JSON.stringify(sandbox.name)} (ID ${sandbox.id})`;
  switch (action) {
    case "inspect": return `Inspect ${target} and help me with it.`;
    case "sleep": return `Put ${target} to sleep. Check its current state and explain whether its work can be saved before proceeding.`;
    case "wake": return `Wake ${target}. Check its saved state and any applicable cost or budget limits first.`;
    case "delete": return `I want to delete ${target}. Explain what data would be lost and ask for my explicit confirmation before deleting it.`;
    case "command": return `Help me run a command on ${target}. Ask what I want to run, then check the current machine and its execution permissions.`;
    case "preview": return `Help me manage preview links for ${target}. Confirm the port and public access implications before creating a link.`;
    case "settings": return `Help me change the settings of ${target}. Inspect its current configuration and ask which settings I want to change.`;
    case "save": return `Help me save or copy the work on ${target}. Inspect its available snapshots and explain the storage and billing requirements before changing anything.`;
    case "desktop": return `Help me use the desktop on ${target}. Check whether its desktop is available and who currently has control before interacting with it.`;
  }
}
