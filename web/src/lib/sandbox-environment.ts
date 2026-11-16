// Pure environment-recipe draft parsing shared by the sandbox create/copy
// forms and unit tests — no JSX, no fetch, no CSS imports.

export type SandboxEnvironment = {
	repository?: { url: string; ref?: string };
	setupCommand?: string;
	setupTimeoutSeconds?: number;
	env?: Record<string, string>;
	secretRefs?: Record<string, string>;
};

export type SandboxEnvironmentDraft = {
	repositoryUrl: string;
	repositoryRef: string;
	setupCommand: string;
	setupTimeoutSeconds: string;
	envJson: string;
	secretRefs: Record<string, string>;
};

export const emptyEnvironmentDraft: SandboxEnvironmentDraft = {
	repositoryUrl: "", repositoryRef: "", setupCommand: "",
	setupTimeoutSeconds: "", envJson: "", secretRefs: {},
};

const envKey = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
const secretID = /^sec_[A-Za-z0-9_-]{1,124}$/;
const utf8 = (value: string) => new TextEncoder().encode(value).length;
const bad = (message: string): never => { throw new Error(message); };

export function parseSandboxEnvironmentDraft(draft: SandboxEnvironmentDraft, restoring = false): SandboxEnvironment | undefined {
	const out: SandboxEnvironment = {};
	const url = draft.repositoryUrl.trim();
	const ref = draft.repositoryRef.trim();
	if (ref && !url) bad("Repository ref requires a repository URL.");
	if (url) {
		if (restoring) bad("Restored sandboxes cannot run repository checkout or setup.");
		if (utf8(url) > 2048) bad("Repository URL is at most 2048 bytes.");
		if (/[\x00-\x1f\x7f]/.test(url)) bad("Repository URL contains control characters.");
		if (/[?#]/.test(url)) bad("Repository URL cannot contain a query or fragment.");
		let parsed: URL;
		try { parsed = new URL(url); } catch { bad("Repository URL must be a valid public HTTPS URL."); }
		if (parsed!.protocol !== "https:" || parsed!.username !== "" || parsed!.password !== "" || parsed!.hostname === "") {
			bad("Repository must be a public HTTPS URL without credentials.");
		}
		const repository: { url: string; ref?: string } = { url };
		if (ref) {
			if (utf8(ref) > 256 || ref.startsWith("-") || /[\s\x00-\x1f\x7f]/.test(ref)) {
				bad("Invalid repository ref.");
			}
			repository.ref = ref;
		}
		out.repository = repository;
	}
	const command = draft.setupCommand;
	if (command.trim()) {
		if (restoring) bad("Restored sandboxes cannot run repository checkout or setup.");
		if (utf8(command) > 32768) bad("Setup command is at most 32KiB.");
		if (command.includes("\0")) bad("Setup command cannot contain NUL bytes.");
		out.setupCommand = command;
	}
	const timeoutRaw = draft.setupTimeoutSeconds.trim();
	if (timeoutRaw) {
		if (out.repository === undefined && out.setupCommand === undefined) {
			bad("Setup timeout requires a repository or setup command.");
		}
		const timeout = Number(timeoutRaw);
		if (!Number.isInteger(timeout) || timeout < 1 || timeout > 3600) {
			bad("Setup timeout must be 1-3600 seconds.");
		}
		out.setupTimeoutSeconds = timeout;
	}
	let env: Record<string, string> | undefined;
	const envJson = draft.envJson.trim();
	if (envJson) {
		let parsed: unknown;
		try { parsed = JSON.parse(envJson); } catch { bad("Environment must be valid JSON."); }
		if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
			bad("Environment must be a JSON object.");
		}
		env = Object.create(null) as Record<string, string>;
		let total = 0;
		for (const [key, value] of Object.entries(parsed as Record<string, unknown>)) {
			if (typeof value !== "string") bad(`Environment value for ${key} must be a string.`);
			if (!envKey.test(key)) bad(`Invalid environment name ${key}.`);
			const text = value as string;
			if (utf8(text) > 8192 || text.includes("\0")) bad(`Environment value for ${key} exceeds the size limit.`);
			total += utf8(key) + utf8(text);
			if (total > 32768) bad("Combined environment exceeds the size limit.");
			env[key] = text;
		}
	}
	let refs: Record<string, string> | undefined;
	const refEntries = Object.entries(draft.secretRefs);
	if (refEntries.length > 0) {
		refs = Object.create(null) as Record<string, string>;
		for (const [key, id] of refEntries) {
			if (!envKey.test(key)) bad(`Invalid secret environment name ${key}.`);
			if (!secretID.test(id)) bad(`Invalid secret reference for ${key}.`);
			if (env && Object.hasOwn(env, key)) {
				bad(`Environment name ${key} cannot be both a value and a secret reference.`);
			}
			refs[key] = id;
		}
	}
	if (Object.keys(env ?? {}).length + Object.keys(refs ?? {}).length > 64) {
		bad("Environment and secret references allow at most 64 combined entries.");
	}
	if (env && Object.keys(env).length > 0) out.env = Object.fromEntries(Object.entries(env));
	if (refs) out.secretRefs = Object.fromEntries(refEntries);
	return Object.keys(out).length > 0 ? out : undefined;
}
