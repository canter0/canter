export type GitHubConnection = { enabled: boolean; connected: boolean; reconnect?: boolean; login?: string; provider?: string; appEnabled?: boolean; installUrl?: string };

export function githubConnectURL(connection: GitHubConnection | undefined | null, workspace: string, next: string) {
  return `/api/canter/auth/oauth/${connection?.appEnabled ? "github-app" : "github"}?${new URLSearchParams({ mode: "repository", workspace, next })}`;
}
