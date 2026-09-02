export type Authority = {
  inspect: boolean;
  draft: boolean;
  applyMode: string;
};

export type Account = {
  id: string;
  email: string;
};

export type Workspace = {
  id: string;
  name: string;
  revision?: number;
};

export type Installation = {
  id: string;
  workspaceId: string;
  name: string;
  harness: string;
  authority: Authority;
  createdAt: string;
  lastSeenAt?: string | null;
  revokedAt?: string | null;
  activeSessions?: number;
};

export type DeviceAuthorization = {
  userCode: string;
  name: string;
  harness: string;
  authority: Authority;
  status: string;
  expiresAt: string;
};

export type Me = {
  account: Account;
  workspaces: Workspace[];
};

export type SystemRecord = {
  workspaceId: string;
  revision: number;
  contract: {
    apiVersion: string;
    kind: string;
    metadata: { name: string };
    spec: {
      intent?: string;
      constraints?: { host?: { class?: string; count?: number; memoryMiB?: number; systemReserveMiB?: number } };
      services?: Array<{
        name: string;
        kind: string;
        engine?: string;
        instances?: number;
        isolation?: string;
        dependsOn?: string[];
        resources?: { vcpu?: number; memoryMiB?: number };
        readiness?: { protocol?: string; port?: number };
        networking?: string;
      }>;
      m1?: { prefix?: string };
    };
  };
  createdAt: string;
  updatedAt: string;
};

export type SystemView = {
  schemaVersion: string;
  contract: SystemRecord["contract"];
  graph: {
    nodes: Array<{ id: string; kind: string; placement?: string; properties?: Record<string, string> }>;
    invariants: Array<{ kind: string; subject: string; value: string }>;
    capacity: { hostMemoryMiB: number; systemReserveMiB: number; guestMemoryMiB: number; unallocatedMemoryMiB: number };
  };
  bindings: Array<{ service: string; kind: string; engine: string; environment: string; consumers: string[] }>;
  host?: { phase?: string; class?: string; resources?: Array<{ name: string; status: string; address?: string }> };
  release?: { release?: { phase?: string; runningVersion?: string; healthy?: boolean } };
	applicationCapacity?: { service: string; mode: string; declaredBaseline: number; maximumReplicas: number; desiredReplicas: number; readyReplicas: number };
  issues?: string[];
};

export type ChangeSummary = {
  id: string;
  system: string;
  summary: string;
  phase: string;
  digest: string;
  createdAt?: string;
  draftedBy?: { id?: string; displayName?: string };
  executionId?: string;
  executionPhase?: string;
};

export type ActorRef = {
  kind: "human" | "agent" | "canter" | string;
  id: string;
  sessionId?: string;
  displayName?: string;
};

export type ExactAuthorization = {
  digest: string;
  authorizedAt: string;
  authorizedBy?: ActorRef;
};

export type InitialDeploymentSummary = {
  id: string;
  system: string;
  summary: string;
  phase: "drafted" | "authorized" | "queued" | "running" | "succeeded" | "failed" | string;
  digest: string;
};

export type InitialDeploymentOperation = {
  id: string;
  kind: string;
  description: string;
  phase: string;
  startedAt?: string;
  completedAt?: string;
  failure?: string;
};

export type InitialDeploymentDetail = InitialDeploymentSummary & {
  schemaVersion: string;
  workspaceId: string;
  draftedBy: ActorRef;
  authorization?: ExactAuthorization;
  plan: {
    system: SystemRecord["contract"];
    artifactSha256: string;
    release: {
      command: string[];
      environment?: Record<string, string>;
      healthPath: string;
      publicPort: number;
    };
    verification: {
      method: string;
      path: string;
      expectedStatus: number;
      bodyContains?: string;
    };
    workspaceRevision: number;
    replacesDeploymentId?: string;
  };
  operations: InitialDeploymentOperation[];
  evidence?: Array<{
    operationId: string;
    kind: string;
    statement: string;
    observedAt: string;
  }>;
  failure?: string;
  createdAt: string;
  updatedAt: string;
  completedAt?: string;
};

export type InitialDeploymentExecution = {
  id: string;
  workspaceId: string;
  deploymentId: string;
  system: string;
  phase: "queued" | "running" | "succeeded" | "failed" | string;
  requestedBy: ActorRef;
  attempts: number;
  availableAt: string;
  claimedBy?: string;
  leaseExpiresAt?: string;
  failure?: string;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
};

export type ChangeDetail = ChangeSummary & {
  plan?: {
    baseVersion?: string;
	scale?: {
	  service: string;
	  fromReplicas: number;
	  toReplicas: number;
	  capacityMode: string;
	  leaseSeconds?: number;
	  restoreAt?: string;
	  restoreToReplicas?: number;
	};
    impact?: {
      affectedServices?: string[];
      availability?: string;
      data?: string;
      monthlyCostDeltaCents?: number;
    };
  };
  operations?: Array<{
    id: string;
    kind: string;
    description: string;
    reversibility: string;
    phase: string;
    attempts?: number;
  }>;
  evidence?: Array<{
    operationId: string;
    kind: string;
    statement: string;
    observedAt: string;
  }>;
  execution?: {
    id: string;
    workspaceId: string;
    system: string;
    changeId: string;
    phase: string;
    requestedBy: ActorRef;
    attempts: number;
    availableAt: string;
    claimedBy?: string;
    leaseExpiresAt?: string;
    failure?: string;
    createdAt: string;
    startedAt?: string;
    completedAt?: string;
  };
};

export type ChangeApprovalCapability = {
  id: string;
  workspaceId: string;
  system: string;
  changeId: string;
  digest: string;
  action: "authorize-and-apply" | string;
  requestedBy: Installation;
  createdAt: string;
  expiresAt: string;
  consumedAt?: string;
  executionId?: string;
};

export type ChangeApprovalReview = {
  capability: ChangeApprovalCapability;
  change: ChangeDetail;
  canApprove: boolean;
};

export type ChangeApprovalResult = {
  capability: ChangeApprovalCapability;
  change: ChangeDetail;
  execution: {
    id: string;
    phase: string;
    system: string;
    changeId: string;
  };
};

export type StandingPolicyEnvelope = {
  allowedInstallationIds: string[];
  affectedServices: string[];
  operationKinds: string[];
  availability: string[];
  data: string[];
  allowedReversibility: string[];
  maxAdditionalMonthlyCostCents: number;
  maxOperations: number;
	scaleLimits?: Record<string, { min: number; max: number }>;
	maxScaleDurationSeconds?: number;
	allowPermanentScale?: boolean;
};

export type StandingPolicy = {
  id: string;
  workspaceId: string;
  system: string;
  name: string;
  description: string;
  digest: string;
  envelope: StandingPolicyEnvelope;
  workspaceRevision: number;
  systemRevision: number;
  createdByAccount: string;
  createdAt: string;
  expiresAt: string;
  revokedAt?: string;
  revokedByAccount?: string;
};

type APIErrorBody = { error?: string | { message?: string }; message?: string };

export class CanterAPIError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "CanterAPIError";
    this.status = status;
  }
}

export async function canterFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has("content-type")) headers.set("content-type", "application/json");
  const response = await fetch(`/api/canter${path}`, {
    ...init,
    headers,
    credentials: "include",
    cache: "no-store",
  });
  if (!response.ok) {
    let body: APIErrorBody = {};
    try {
      body = await response.json() as APIErrorBody;
    } catch {
      // The status text is the final fallback for non-JSON failures.
    }
    const message = typeof body.error === "string" ? body.error : body.error?.message;
    throw new CanterAPIError(response.status, message ?? body.message ?? response.statusText);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export function relativeTime(value?: string | null): string {
  if (!value) return "never";
  const elapsed = Date.now() - new Date(value).getTime();
  if (elapsed < 60_000) return "now";
  if (elapsed < 3_600_000) return `${Math.floor(elapsed / 60_000)} min`;
  if (elapsed < 86_400_000) return `${Math.floor(elapsed / 3_600_000)} hr`;
  return `${Math.floor(elapsed / 86_400_000)} days`;
}

export function authorityLabel(authority: Authority): string {
  if (authority.draft) return "inspect + draft";
  if (authority.inspect) return "inspect";
  return "none";
}
