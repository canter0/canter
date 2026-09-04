# Engine boundary

Canter accepts a provider-neutral desired state. The initial public primitives are `compute` and `m1`; implementation vendors are deliberately absent from the spec, plan, CLI output, and receipts.

The execution path is:

1. Parse and validate `canter.yaml` locally.
2. Send only the validated, credential-free intent to the model.
3. Decode the response into a strict typed plan and reject unknown or changed operations.
4. Resolve public classes and image aliases through private adapters.
5. Persist every returned resource ID before waiting on it.
6. Reconcile the resource to ACTIVE. Exhausted network attempts are recorded and deleted before retry.
7. Require the resource itself to upload a boot proof through a short-lived signed `m1` URL. A nonzero bootstrap becomes an explicit `failed` state with its error tail, never `ready` or an indefinite `creating` state.
8. Mark the sandbox ready and write an immutable operation receipt only after the proof exists.
9. Destroy only IDs recovered from persisted sandbox state, then retain the proof, state, and teardown receipt.

The model is therefore an intent compiler, not a privileged infrastructure actor. The deterministic engine remains the authority for policy, mutation, reconciliation, and evidence.
