# Workspace secrets

Canter stores shared credentials once per workspace. Workspace owners can add, rotate, or remove them; workspace members and agents with inspect permission can list metadata. There is no API to reveal a saved value, and no agent tool to export one. These credentials do not change an agent's infrastructure authority.

## Supported use

- **OpenRouter / hosted agent:** the owner explicitly selects this use while saving. All hosted conversations in this workspace use that credential. Each provider request resolves the current value, and pins the URL to `https://openrouter.ai/api/v1`. The value appears only in the server's authorization header. Redirects are refused. The workspace key overrides the server's default model connection. Model charges go to the key's OpenRouter account and are not included in Canter's resource bill. Configure a provider spending limit before connecting a key.
- **Store for later:** encrypted storage only. These values are not injected into agent environments, model messages, application configuration, or external agents. Adding a key does not implement the corresponding provider integration.
- **AWS:** not implemented. Prefer a dedicated IAM role with workload federation/OIDC and short-lived STS credentials. Do not turn an AWS root login into a workspace secret. A future connector should expose approved AWS operations with account/resource/action scopes, not a generic credential-export endpoint.
- **External agents:** retain their own existing grants. They can inspect secret metadata but cannot fetch values or use the hosted operator's OpenRouter credential. A future broker should authorize a specific provider operation for a specific installation and use the credential server-side.

## Storage and deployment

`CANTER_SECRETS_KEY_FILE` points to a private regular file (mode 0600 or 0400), outside the repository and database, containing:

```json
{"active":"v1","keys":{"v1":"<base64-encoded 32 random bytes>"}}
```

Generate cryptographic random bytes, never a password. The local development keyring lives in ignored `.secrets/`; production must provision its own keyring through the deployment's secret manager or protected secret mount. Do not ship the local development key. Missing configuration disables new saves and rotations; owners can still remove existing ciphertext. A missing/wrong decryption key causes a provider error, never a silent fallback to another account while an active workspace connection exists.

Encryption is AES-256-GCM with a fresh random nonce for every write. Authenticated context binds the ciphertext to its workspace, record ID, purpose, and schema domain. The browser receives only metadata. Plaintext is not included in request responses, audit metadata, model prompts, operator checkpoints, or error messages. Form drafts are not persisted in browser storage. TLS is required for production ingress; the loopback HTTP development server is not a production endpoint.

Encryption protects database copies from revealing values without the separate key. It does not protect against a compromised running control plane with access to both the keyring and the database. Server memory and provider requests necessarily contain credentials briefly. Use a managed KMS/HSM or external secrets manager for stronger production key custody; this implementation does not claim hardware isolation or that credentials are impossible to recover after server compromise.

## Lifecycle

Creation, replacement, removal, and server-side use produce audit records containing IDs and actions, without secret values. Metadata shows the last use attempt and update time. A transaction makes writes and their audit records atomic. Optimistic versions reject stale rotation/removal. At-rest ciphertext is deleted on removal, while the metadata/audit history remains. Removal affects future resolutions; requests already sent may finish. It does not invalidate the key at its original provider. After removal of an OpenRouter connection, the hosted operator uses the server default if configured.

To rotate the encryption key, add a new key ID, set `active` to it, retain old IDs for decryption, and restart the control plane. New or replaced values use the active key. Replace each saved value (using the original provider credential or a newly issued one) to re-encrypt existing records before retiring an old key. There is no automatic bulk re-encryption job in this version. Back up the keyring independently and securely; losing it makes saved values unrecoverable.

Provider-key expiry and automatic issuance/rotation are not yet implemented. Use provider-native expiration and manual replacement. Workspace-wide use is deliberately explicit; this version has no personal-secret scope or per-installation grants.

## Evidence and references

`secrets_test.go` covers randomized encryption, tampering, workspace/record/purpose binding, keyring rotation, anonymous and cross-origin denial, owner-only mutations, agent/cookie confusion, metadata-only reads, provider endpoint binding, fail-closed decryption, optimistic rotation, removal, and audit redaction.

- [OWASP Secrets Management](https://cheatsheetseries.owasp.org/cheatsheets/Secrets_Management_Cheat_Sheet.html): least privilege, lifecycle, audit, and separating encryption keys from stored secrets.
- [AWS IAM security best practices](https://docs.aws.amazon.com/IAM/latest/UserGuide/best-practices.html): temporary workload credentials and least-privilege access.
