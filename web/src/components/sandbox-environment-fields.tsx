"use client";

import { useEffect, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import dialogStyles from "./agent-connection-dialog.module.css";
import {
	emptyEnvironmentDraft, parseSandboxEnvironmentDraft,
	type SandboxEnvironment, type SandboxEnvironmentDraft,
} from "@/lib/sandbox-environment";

export { emptyEnvironmentDraft, parseSandboxEnvironmentDraft };
export type { SandboxEnvironment, SandboxEnvironmentDraft };

type ApprovedSecret = { id: string; name: string };
type SecretState = { workspaceId: string; secrets: ApprovedSecret[] } | { workspaceId: string; error: true } | null;

// SandboxEnvironmentFields renders the optional recipe section shared by the
// create and copy dialogs. Repository/setup fields are only shown for a new
// machine; secret selection uses approved-secret metadata — IDs only, never
// values. Metadata failures do not block a plain create.
export default function SandboxEnvironmentFields({
	workspaceId, draft, onChange, restoring, disabled,
}: {
	workspaceId: string;
	draft: SandboxEnvironmentDraft;
	onChange: (d: SandboxEnvironmentDraft) => void;
	restoring: boolean;
	disabled: boolean;
}) {
	const [state, setState] = useState<SecretState>(null);
	const [attempt, setAttempt] = useState(0);
	useEffect(() => {
		const controller = new AbortController();
		canterFetch<{ secrets: { id: string; name: string; purpose: string; sandboxAllowed: boolean }[] }>(
			`/workspaces/${encodeURIComponent(workspaceId)}/secrets`, { signal: controller.signal },
		).then(result => {
			if (!controller.signal.aborted) {
				setState({ workspaceId, secrets: result.secrets.filter(s => s.purpose === "stored" && s.sandboxAllowed) });
			}
		}).catch(() => {
			if (!controller.signal.aborted) setState({ workspaceId, error: true });
		});
		return () => controller.abort();
	}, [workspaceId, attempt]);
	const set = (patch: Partial<SandboxEnvironmentDraft>) => onChange({ ...draft, ...patch });
	// Results are scoped to the workspace that produced them so an
	// organization switch never renders stale choices.
	const secrets = state !== null && "secrets" in state && state.workspaceId === workspaceId ? state.secrets : null;
	const fetchFailed = state !== null && "error" in state && state.workspaceId === workspaceId;
	return <details className={dialogStyles.field}>
		<summary>Environment setup (optional)</summary>
		<fieldset disabled={disabled}>
			<p className={dialogStyles.muted ?? ""}>Environment values apply to setup and command jobs.</p>
			{!restoring ? <>
				<label className={dialogStyles.field}>Repository<input type="url" value={draft.repositoryUrl} onChange={e => set({ repositoryUrl: e.target.value })} placeholder="https://github.com/org/repo" maxLength={2048} /></label>
				<label className={dialogStyles.field}>Ref<input type="text" value={draft.repositoryRef} onChange={e => set({ repositoryRef: e.target.value })} placeholder="branch, tag, or commit SHA" maxLength={256} /></label>
				<label className={dialogStyles.field}>Setup command<textarea value={draft.setupCommand} onChange={e => set({ setupCommand: e.target.value })} rows={2} placeholder="make setup" /></label>
				<label className={dialogStyles.field}>Setup timeout seconds<input type="number" value={draft.setupTimeoutSeconds} onChange={e => set({ setupTimeoutSeconds: e.target.value })} min={1} max={3600} placeholder="1800" /></label>
			</> : null}
			<label className={dialogStyles.field}>Environment <span>(optional JSON object — nonsecret values only)</span><textarea value={draft.envJson} onChange={e => set({ envJson: e.target.value })} placeholder={'{"NODE_ENV": "test"}'} rows={2} style={{ fontFamily: "monospace", fontSize: 12 }} /></label>
			{fetchFailed ? <p>Couldn’t load approved secrets. <button type="button" onClick={() => setAttempt(a => a + 1)}>Retry</button></p>
				: secrets === null ? <p>Loading approved secrets…</p>
				: secrets.length === 0 ? <p>No organization secrets are enabled for sandbox use.</p>
				: secrets.map(secret => (
					<label key={secret.id} className={dialogStyles.field} style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
						<input type="checkbox" checked={draft.secretRefs[secret.name] === secret.id} onChange={e => {
							const next = { ...draft.secretRefs };
							if (e.target.checked) next[secret.name] = secret.id; else delete next[secret.name];
							set({ secretRefs: next });
						}} />
						<span>Use {secret.name} from approved secrets</span>
					</label>
				))}
		</fieldset>
	</details>;
}
