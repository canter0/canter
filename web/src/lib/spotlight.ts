import type { WorkspaceIconName } from "../components/workspace-icon";

export type SpotlightCommand = {
  id: string;
  title: string;
  key?: string;
  icon: WorkspaceIconName;
  run: () => void;
};

export function searchCommands(commands: readonly SpotlightCommand[], input: string) {
  const terms = input.trim().toLowerCase().split(/\s+/).filter(Boolean);
  return commands.filter(command => terms.every(term => `${command.title} ${command.key ?? ""}`.toLowerCase().includes(term)));
}
