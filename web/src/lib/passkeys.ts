import { canterFetch } from "./canter-api";

type DescriptorJSON = Omit<PublicKeyCredentialDescriptor, "id"> & {
  id: string;
};
type RequestJSON = Omit<
  PublicKeyCredentialRequestOptions,
  "challenge" | "allowCredentials"
> & { challenge: string; allowCredentials?: DescriptorJSON[] };
type CreationJSON = Omit<
  PublicKeyCredentialCreationOptions,
  "challenge" | "user" | "excludeCredentials"
> & {
  challenge: string;
  user: Omit<PublicKeyCredentialUserEntity, "id"> & { id: string };
  excludeCredentials?: DescriptorJSON[];
};
function decode(value: string): ArrayBuffer {
  const raw = atob(value.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (char) => char.charCodeAt(0)).buffer;
}
function encode(value: ArrayBuffer): string {
  return btoa(String.fromCharCode(...new Uint8Array(value)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}
function credentialJSON(credential: PublicKeyCredential) {
  const response = credential.response;
  const common = {
    id: credential.id,
    rawId: encode(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment,
    clientExtensionResults: credential.getClientExtensionResults(),
  };
  if (response instanceof AuthenticatorAttestationResponse)
    return {
      ...common,
      response: {
        clientDataJSON: encode(response.clientDataJSON),
        attestationObject: encode(response.attestationObject),
        transports: response.getTransports?.() ?? [],
      },
    };
  const assertion = response as AuthenticatorAssertionResponse;
  return {
    ...common,
    response: {
      clientDataJSON: encode(assertion.clientDataJSON),
      authenticatorData: encode(assertion.authenticatorData),
      signature: encode(assertion.signature),
      userHandle: assertion.userHandle ? encode(assertion.userHandle) : null,
    },
  };
}
export async function authenticatePasskey(
  kind: "login" | "reauth",
  signal?: AbortSignal,
  conditional = false,
) {
  const { publicKey } = await canterFetch<{ publicKey: RequestJSON }>(
    `/auth/passkeys/${kind}/begin`,
    { method: "POST", body: "{}", signal },
  );
  const credential = (await navigator.credentials.get({
    publicKey: {
      ...publicKey,
      challenge: decode(publicKey.challenge),
      allowCredentials: publicKey.allowCredentials?.map((item) => ({
        ...item,
        id: decode(item.id),
      })),
    },
    signal,
    mediation: conditional ? "conditional" : "optional",
  })) as PublicKeyCredential | null;
  if (!credential || signal?.aborted)
    throw new DOMException("Passkey cancelled", "AbortError");
  return canterFetch(`/auth/passkeys/${kind}/finish`, {
    method: "POST",
    body: JSON.stringify(credentialJSON(credential)),
    signal,
  });
}
export async function registerPasskey(name: string) {
  const { publicKey } = await canterFetch<{ publicKey: CreationJSON }>(
    "/auth/passkeys/register/begin",
    { method: "POST", body: JSON.stringify({ name }) },
  );
  const credential = (await navigator.credentials.create({
    publicKey: {
      ...publicKey,
      challenge: decode(publicKey.challenge),
      user: { ...publicKey.user, id: decode(publicKey.user.id) },
      excludeCredentials: publicKey.excludeCredentials?.map((item) => ({
        ...item,
        id: decode(item.id),
      })),
    },
  })) as PublicKeyCredential | null;
  if (!credential) throw new Error("Passkey setup was cancelled.");
  return canterFetch("/auth/passkeys/register/finish", {
    method: "POST",
    body: JSON.stringify(credentialJSON(credential)),
  });
}
