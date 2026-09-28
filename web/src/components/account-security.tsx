"use client";
import Link from "next/link";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { SettingsShell, SettingRow } from "./settings-shell";
import { canterFetch, CanterAPIError } from "@/lib/canter-api";
import { registerPasskey, authenticatePasskey } from "@/lib/passkeys";
import styles from "./settings.module.css";

type Security = {
  providers: string[];
  totp: boolean;
  totpAvailable: boolean;
  passkeysAvailable: boolean;
  hasPassword: boolean;
  recoveryCodesRemaining: number;
  recentAuth: boolean;
  passkeys: {
    id: string;
    name: string;
    createdAt: string;
    lastUsedAt: string | null;
  }[];
  sessions: {
    id: string;
    userAgent: string;
    ip: string;
    current: boolean;
    lastSeenAt: string;
    expiresAt: string;
  }[];
  events: { action: string; at: string }[];
};
export function AccountSecurity() {
  const [data, setData] = useState<Security | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const recoveryRef = useRef<HTMLElement>(null);
  const setupCodeRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLDivElement>(null);
  const [setup, setSetup] = useState<{ secret: string; qr: string } | null>(
    null,
  );
  const [codes, setCodes] = useState<string[]>([]);
  const [confirm, setConfirm] = useState<{
    label: string;
    route: string;
  } | null>(null);
  const [showReauth, setShowReauth] = useState(false);
  useEffect(() => {
    if (codes.length) recoveryRef.current?.focus();
  }, [codes]);
  useEffect(() => {
    if (setup) setupCodeRef.current?.focus();
  }, [setup]);
  useEffect(() => {
    if (confirm) confirmRef.current?.focus();
  }, [confirm]);
  const reload = useCallback(async () => {
    const next = await canterFetch<Security>("/auth/security");
    setData(next);
    setShowReauth(!next.recentAuth);
  }, []);
  useEffect(() => {
    let active = true;
    canterFetch<Security>("/auth/security")
      .then((value) => {
        if (active) {
          setData(value);
          setShowReauth(!value.recentAuth);
        }
      })
      .catch((cause) => {
        if (active)
          setError(
            cause instanceof CanterAPIError && cause.status === 404
              ? "Account security is temporarily unavailable. Please try again."
              : cause instanceof Error
              ? cause.message
              : "Could not load security settings.",
          );
      });
    return () => {
      active = false;
    };
  }, []);
  async function run(action: () => Promise<void>) {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await action();
      await reload();
    } catch (cause) {
      if (cause instanceof CanterAPIError && cause.status === 428)
        setShowReauth(true);
      setError(
        cause instanceof Error
          ? cause.message
          : "Could not update account security.",
      );
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  }
  async function mutate(
    route: string,
    body: Record<string, string> = {},
    method = "POST",
  ) {
    return canterFetch<{ recoveryCodes?: string[] }>(
      `/auth/security/${route}`,
      { method, body: JSON.stringify(body) },
    );
  }
  function submit(
    event: FormEvent<HTMLFormElement>,
    action: (form: FormData) => Promise<void>,
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    void run(async () => {
      await action(values);
      form.reset();
    });
  }
  return (
    <SettingsShell
      active="Security"
      title="Security"
      description="Manage your sign-in methods, recovery options, and active sessions."
    >
      {error ? (
        <p role="alert" className={`${styles.notice} ${styles.error}`}>
          {error}
          {!data ? (
            <button disabled={busy} onClick={() => void run(async () => {})}>Try again</button>
          ) : null}
        </p>
      ) : null}
      {message ? (
        <p role="status" className={styles.notice}>
          {message}
        </p>
      ) : null}
      {!data ? (
        !error ? <p role="status">Loading account security…</p> : null
      ) : (
        <>
          <section className={styles.section}>
            <h2>Confirm your identity</h2>
            <div className={styles.card}>
              <SettingRow
                title={showReauth ? "Verification needed" : "Recently verified"}
                description="Security changes require a recent sign-in. Changes to credentials sign out your other sessions."
              >
                <button
                  className={styles.button}
                  onClick={() => setShowReauth((value) => !value)}
                >
                  {showReauth ? "Hide" : "Verify again"}
                </button>
              </SettingRow>
              {showReauth ? (
                <div className="pb-5">
                  {data.hasPassword || data.totp ? (
                    <form
                      className={styles.form}
                      onSubmit={(event) =>
                        submit(event, async (form) => {
                          await mutate("reauth", {
                            password: String(form.get("password") ?? ""),
                            code: String(form.get("code") ?? ""),
                          });
                          setShowReauth(false);
                          setMessage(
                            "Identity confirmed. You can now update security settings.",
                          );
                        })
                      }
                    >
                      {data.hasPassword ? (
                        <label>
                          Current password
                          <input
                            name="password"
                            type="password"
                            autoComplete="current-password"
                            required
                          />
                        </label>
                      ) : null}
                      {data.totp ? (
                        <label>
                          Authenticator or recovery code
                          <input
                            name="code"
                            autoComplete="one-time-code"
                            required
                          />
                        </label>
                      ) : null}
                      <div className={styles.actions}>
                        <button className={styles.primary} disabled={busy}>
                          Confirm identity
                        </button>
                      </div>
                    </form>
                  ) : null}
                  <div className="mt-4 flex flex-wrap items-center gap-3">
                    {data.passkeys.length ? (
                      <button
                        disabled={busy}
                        className={styles.button}
                        onClick={() =>
                          void run(async () => {
                            await authenticatePasskey("reauth");
                            setShowReauth(false);
                            setMessage("Identity confirmed with your passkey.");
                          })
                        }
                      >
                        Use a passkey
                      </button>
                    ) : null}
                    {data.providers.map((provider) => (
                      <a
                        key={provider}
                        href={`/api/canter/auth/oauth/${provider}?mode=sign-in&next=%2Fapp%2Faccount%2Fsecurity`}
                        className="text-xs underline"
                      >
                        Sign in with{" "}
                        {provider === "google" ? "Google" : "GitHub"} again
                      </a>
                    ))}
                  </div>
                </div>
              ) : null}
            </div>
          </section>
          {codes.length ? (
            <section
              ref={recoveryRef}
              tabIndex={-1}
              className={styles.section}
              aria-label="Save recovery codes"
            >
              <h2>Save your recovery codes</h2>
              <div className={`${styles.card} py-5`}>
                <p className="mb-4 leading-6">
                  Each code can replace an authenticator code once. Save these
                  somewhere safe now; Canter cannot show them again.
                </p>
                <pre className="overflow-x-auto rounded-lg bg-[#111] p-4 text-sm leading-7 select-all">
                  {codes.join("\n")}
                </pre>
                <div className="mt-4 flex flex-wrap gap-3">
                  <button
                    className={styles.button}
                    onClick={() => {
                      const blob = new Blob(
                        [
                          "Canter recovery codes — keep private. Each code works once.\n\n" +
                            codes.join("\n"),
                        ],
                        { type: "text/plain" },
                      );
                      const url = URL.createObjectURL(blob);
                      const link = document.createElement("a");
                      link.href = url;
                      link.download = "canter-recovery-codes.txt";
                      link.click();
                      setTimeout(() => URL.revokeObjectURL(url), 1000);
                    }}
                  >
                    Download codes
                  </button>
                  <button
                    className={styles.primary}
                    onClick={() => setCodes([])}
                  >
                    I saved these codes
                  </button>
                </div>
              </div>
            </section>
          ) : null}
          <section className={styles.section}>
            <h2>Passkeys</h2>
            <div className={styles.card}>
              <SettingRow
                title="Sign in with your device"
                description="Use Touch ID, Face ID, your password manager, or a security key. Canter never receives your biometric data."
              >
                <span className={styles.badge}>
                  {data.passkeys.length} registered
                </span>
              </SettingRow>
              {data.passkeys.map((key) => (
                <div
                  className="border-b border-[#333] py-4 last:border-0"
                  key={key.id}
                >
                  <form
                    className={styles.form}
                    onSubmit={(event) =>
                      submit(event, async (form) => {
                        await mutate(
                          `passkeys/${key.id}`,
                          { name: String(form.get("name")) },
                          "PATCH",
                        );
                        setMessage("Passkey renamed.");
                      })
                    }
                  >
                    <label>
                      Passkey name
                      <input
                        name="name"
                        defaultValue={key.name}
                        maxLength={80}
                        required
                      />
                    </label>
                    <p className={styles.muted}>
                      Added {new Date(key.createdAt).toLocaleDateString()} ·{" "}
                      {key.lastUsedAt
                        ? `Last used ${new Date(key.lastUsedAt).toLocaleString()}`
                        : "Not used yet"}
                    </p>
                    <div className={styles.actions}>
                      <button disabled={busy} className={styles.button}>
                        Rename
                      </button>
                      <button
                        type="button"
                        disabled={busy}
                        className={styles.button}
                        onClick={() =>
                          setConfirm({
                            label: `Remove “${key.name}”?`,
                            route: `passkeys/${key.id}`,
                          })
                        }
                      >
                        Remove
                      </button>
                    </div>
                  </form>
                </div>
              ))}
              <form
                className={`${styles.form} py-5`}
                onSubmit={(event) =>
                  submit(event, async (form) => {
                    await registerPasskey(String(form.get("name")));
                    setMessage("Passkey added. You can now use it to sign in.");
                  })
                }
              >
                <label>
                  Name a new passkey
                  <input
                    name="name"
                    placeholder="MacBook or security key"
                    maxLength={80}
                    required
                  />
                </label>
                <div className={styles.actions}>
                  <button
                    className={styles.primary}
                    disabled={busy || !data.passkeysAvailable}
                  >
                    Add passkey
                  </button>
                </div>
                {!data.passkeysAvailable ? (
                  <p className={styles.muted}>
                    Passkey setup is temporarily unavailable.
                  </p>
                ) : null}
              </form>
            </div>
          </section>
          <section className={styles.section}>
            <h2>Authenticator app</h2>
            <div className={styles.card}>
              <SettingRow
                title={
                  data.totp
                    ? "Two-factor authentication enabled"
                    : "Add an authenticator"
                }
                description="Use any compatible authenticator app to generate sign-in codes."
              >
                {data.totp ? (
                  <button
                    className={styles.button}
                    disabled={busy}
                    onClick={() =>
                      setConfirm({
                        label:
                          "Remove your authenticator and invalidate its recovery codes?",
                        route: "totp",
                      })
                    }
                  >
                    Remove
                  </button>
                ) : (
                  <button
                    className={styles.button}
                    disabled={busy || !data.totpAvailable}
                    onClick={() =>
                      void run(async () => {
                        setSetup(
                          await canterFetch("/auth/security/totp/start", {
                            method: "POST",
                            body: "{}",
                          }),
                        );
                      })
                    }
                  >
                    Set up
                  </button>
                )}
              </SettingRow>
              {setup ? (
                <div className="pb-5">
                  <p className="mb-4 leading-6">
                    Scan this QR code in your authenticator app, or enter the
                    setup key manually. Setup expires after 10 minutes.
                  </p>
                  {/* The QR is generated locally by Canter; its secret is never sent to an image service. */}
                  {/* eslint-disable-next-line @next/next/no-img-element */}
                  <img
                    src={setup.qr}
                    alt="Authenticator setup QR code"
                    width={256}
                    height={256}
                    className="mb-4 max-w-full rounded-lg"
                  />
                  <p className="mb-5 break-all rounded-lg bg-[#111] p-3 font-mono select-all">
                    {setup.secret}
                  </p>
                  <form
                    className={styles.form}
                    onSubmit={(event) =>
                      submit(event, async (form) => {
                        const result = await mutate("totp/confirm", {
                          code: String(form.get("code")),
                        });
                        setCodes(result.recoveryCodes ?? []);
                        setSetup(null);
                        setMessage(
                          "Authenticator enabled. Save your recovery codes below.",
                        );
                      })
                    }
                  >
                    <label>
                      Enter a code to finish setup
                      <input
                        ref={setupCodeRef}
                        name="code"
                        inputMode="numeric"
                        autoComplete="one-time-code"
                        pattern="[0-9]{6}"
                        maxLength={6}
                        required
                      />
                    </label>
                    <div className={styles.actions}>
                      <button
                        className={styles.button}
                        type="button"
                        onClick={() => setSetup(null)}
                      >
                        Cancel
                      </button>
                      <button disabled={busy} className={styles.primary}>
                        Enable authenticator
                      </button>
                    </div>
                  </form>
                </div>
              ) : null}
              {data.totp ? (
                <SettingRow
                  title="Recovery codes"
                  description={`${data.recoveryCodesRemaining} unused codes. Replacing codes invalidates every previous code.`}
                >
                  <button
                    disabled={busy || codes.length > 0}
                    className={styles.button}
                    onClick={() =>
                      void run(async () => {
                        const result = await mutate("recovery-codes");
                        setCodes(result.recoveryCodes ?? []);
                      })
                    }
                  >
                    Replace codes
                  </button>
                </SettingRow>
              ) : null}
            </div>
          </section>
          {confirm ? (
            <div
              ref={confirmRef}
              tabIndex={-1}
              className={styles.notice}
              role="alert"
            >
              <p>{confirm.label} Your other sessions will be signed out.</p>
              <div className="mt-3 flex gap-3">
                <button
                  className={styles.button}
                  onClick={() => setConfirm(null)}
                >
                  Cancel
                </button>
                <button
                  className={styles.button}
                  disabled={busy}
                  onClick={() =>
                    void run(async () => {
                      await mutate(confirm.route, {}, "DELETE");
                      setConfirm(null);
                      setMessage("Sign-in method removed.");
                    })
                  }
                >
                  Confirm removal
                </button>
              </div>
            </div>
          ) : null}
          <section className={styles.section}>
            <h2>Password</h2>
            <div className={`${styles.card} py-5`}>
              <form
                className={styles.form}
                onSubmit={(event) =>
                  submit(event, async (form) => {
                    await mutate("password", {
                      password: String(form.get("password")),
                    });
                    setMessage(
                      "Password updated. Your other sessions have been signed out.",
                    );
                  })
                }
              >
                <label>
                  {data.hasPassword ? "New password" : "Set a password"}
                  <input
                    name="password"
                    type="password"
                    autoComplete="new-password"
                    maxLength={2048}
                    required
                  />
                  <small>
                    At least 15 characters. Use a unique password or passphrase.
                  </small>
                </label>
                <div className={styles.actions}>
                  <button className={styles.primary} disabled={busy}>
                    Update password
                  </button>
                </div>
              </form>
            </div>
          </section>
          <section className={styles.section}>
            <h2>Active sessions</h2>
            <div className={styles.card}>
              {data.sessions.map((session) => (
                <SettingRow
                  key={session.id}
                  title={
                    session.current
                      ? "This browser"
                      : session.userAgent || "Browser session"
                  }
                  description={`${session.ip || "Location unavailable"} · Active ${new Date(session.lastSeenAt).toLocaleString()}`}
                >
                  {session.current ? (
                    <span className={styles.badge}>Current</span>
                  ) : (
                    <button
                      disabled={busy}
                      className={styles.button}
                      onClick={() =>
                        void run(async () => {
                          await mutate(`sessions/${session.id}`, {}, "DELETE");
                          setMessage("Session signed out.");
                        })
                      }
                    >
                      Sign out
                    </button>
                  )}
                </SettingRow>
              ))}
              <SettingRow title="Sign out other sessions">
                <button
                  className={styles.button}
                  disabled={busy || data.sessions.length < 2}
                  onClick={() =>
                    void run(async () => {
                      await mutate("sessions/others", {}, "DELETE");
                      setMessage("Other sessions signed out.");
                    })
                  }
                >
                  Sign out others
                </button>
              </SettingRow>
            </div>
          </section>
          <section className={styles.section}>
            <h2>Recent security activity</h2>
            <div className={styles.card}>
              {data.events.length ? (
                data.events.map((event, index) => (
                  <SettingRow key={`${event.at}-${index}`} title={event.action}>
                    <time dateTime={event.at}>
                      {new Date(event.at).toLocaleString()}
                    </time>
                  </SettingRow>
                ))
              ) : (
                <p className="py-5 text-[#aaa]">No recent security events.</p>
              )}
            </div>
          </section>
          <Link className="text-xs underline" href="/app/account/connections">
            Manage connected sign-in accounts
          </Link>
        </>
      )}
    </SettingsShell>
  );
}
