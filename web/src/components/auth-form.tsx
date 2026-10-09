"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";
import { canterFetch } from "@/lib/canter-api";
import { authDestination, authError, type AuthProviders } from "@/lib/auth";
import { ProviderIcon } from "./provider-icon";
import { finishAcquisition } from "@/lib/acquisition";
import { authenticatePasskey } from "@/lib/passkeys";
import { AuthBotCheck } from "./auth-bot-check";
import { MorphLabel } from "./conversation-motion";
import { MotionHeight } from "./motion-height";

const inputClass =
  "h-11 w-full rounded-lg border border-[#444] bg-[#202020] px-3 text-[16px] outline-none transition-colors placeholder:text-[#999] focus:border-[#bacbc0] focus:ring-1 focus:ring-[#bacbc0]";
type Stage =
  | "start"
  | "password"
  | "signup"
  | "verify"
  | "mfa"
  | "reset"
  | "reset-complete";
type Config = { email: boolean; passkeys: boolean; turnstileSiteKey: string };
export function AuthForm({
  mode,
  next = "",
  initialError = "",
  accountDeleted = false,
}: {
  mode: "sign-in" | "create-account" | "reset-password";
  next?: string;
  initialError?: string;
  accountDeleted?: boolean;
}) {
  const router = useRouter();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState(authError(initialError));
  const [stage, setStage] = useState<Stage>("start");
  const [email, setEmail] = useState("");
  const [providers, setProviders] = useState<AuthProviders | null>(null);
  const [config, setConfig] = useState<Config | null>(null);
  const [loadError, setLoadError] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const [botAttempt, setBotAttempt] = useState(0);
  const [botToken, setBotToken] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  const emailId = useId(),
    passwordId = useId(),
    codeId = useId(),
    factorId = useId();
  const formRef = useRef<HTMLFormElement>(null);
  const conditional = useRef<AbortController | null>(null);
  const busy = useRef(false);
  const create = mode === "create-account",
    reset = mode === "reset-password";
  const destination = authDestination(next, create ? "/app?welcome=1" : "/app");
  useEffect(() => {
    let cancelled = false;
    Promise.all([
      canterFetch<AuthProviders>("/auth/providers"),
      canterFetch<Config>("/auth/config"),
      canterFetch<{ stage: Stage; email?: string }>("/auth/challenge"),
    ])
      .then(([p, c, challenge]) => {
        if (cancelled) return;
        setProviders(p);
        setConfig(c);
        setLoadError(false);
        if (
          (reset && challenge.stage === "reset") ||
          (create && challenge.stage === "signup") ||
          (!reset && !create && ["verify", "mfa"].includes(challenge.stage))
        ) {
          setStage(challenge.stage);
          setEmail(challenge.email ?? "");
        }
      })
      .catch(() => {
        if (!cancelled) setLoadError(true);
      });
    return () => {
      cancelled = true;
    };
  }, [attempt, create, reset]);
  useEffect(() => {
    if (stage !== "start")
      formRef.current
        ?.querySelector<HTMLInputElement>(
          stage === "password"
            ? 'input[name="password"]'
            : 'input[name="code"]',
        )
        ?.focus();
  }, [stage]);
  useEffect(() => {
    if (!cooldown) return;
    const timer = window.setTimeout(
      () => setCooldown((value) => Math.max(0, value - 1)),
      1000,
    );
    return () => clearTimeout(timer);
  }, [cooldown]);
  useEffect(() => {
    if (
      create ||
      reset ||
      stage !== "start" ||
      !config?.passkeys ||
      !window.PublicKeyCredential
    )
      return;
    const controller = new AbortController();
    conditional.current = controller;
    void (async () => {
      if (
        !(await PublicKeyCredential.isConditionalMediationAvailable?.()) ||
        controller.signal.aborted
      )
        return;
      try {
        await authenticatePasskey("login", controller.signal, true);
        if (!controller.signal.aborted && !busy.current) {
          router.push(destination);
          router.refresh();
        }
      } catch {
        /* Autofill is optional; explicit login remains available. */
      }
    })();
    return () => controller.abort();
  }, [create, reset, stage, config?.passkeys, destination, router]);
  function startBusy() {
    if (busy.current) return false;
    busy.current = true;
    setPending(true);
    setError("");
    conditional.current?.abort();
    return true;
  }
  function stopBusy() {
    busy.current = false;
    setPending(false);
    setBotAttempt((value) => value + 1);
  }
  async function startEmail() {
    const result = await canterFetch<{ stage: Stage }>(
      reset ? "/auth/password/reset/start" : "/auth/signup/start",
      { method: "POST", body: JSON.stringify({ email, botToken }) },
    );
    setStage(result.stage);
    setCooldown(60);
  }
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy.current) return;
    setError("");
    if (stage === "start" && !create && !reset) {
      conditional.current?.abort();
      setStage("password");
      return;
    }
    if (!startBusy()) return;
    const data = new FormData(event.currentTarget);
    try {
      await finishAcquisition();
      if (stage === "start") {
        await startEmail();
        return;
      }
      const route = {
        password: "/auth/signin",
        signup: "/auth/signup/finish",
        verify: "/auth/verify/finish",
        mfa: "/auth/mfa/finish",
        reset: "/auth/password/reset/finish",
        "reset-complete": "",
      }[stage];
      const result = await canterFetch<{
        stage: Stage | "complete";
        email?: string;
      }>(route, {
        method: "POST",
        body: JSON.stringify({
          email,
          password: String(data.get("password") ?? ""),
          code: String(data.get("code") ?? "").trim(),
          factor: String(data.get("factor") ?? "").trim(),
          inviteKey: String(data.get("invite") ?? ""),
          botToken,
        }),
      });
      if (result.stage === "complete") {
        router.push(destination);
        router.refresh();
      } else {
        setStage(result.stage);
        if (result.email) setEmail(result.email);
      }
    } catch (cause) {
      setError(
        cause instanceof Error
          ? cause.message === "unauthorized"
            ? "The email or password is incorrect."
            : cause.message
          : "We couldn't complete your request.",
      );
    } finally {
      stopBusy();
    }
  }
  async function passkeyLogin() {
    if (!startBusy()) return;
    try {
      await finishAcquisition();
      await authenticatePasskey("login");
      router.push(destination);
      router.refresh();
    } catch (cause) {
      setError(
        cause instanceof DOMException &&
          ["NotAllowedError", "AbortError"].includes(cause.name)
          ? "Passkey sign-in was cancelled. Try again or use another method."
          : cause instanceof Error
            ? cause.message
            : "Passkey sign-in failed.",
      );
    } finally {
      stopBusy();
    }
  }
  async function startProvider(
    event: React.MouseEvent<HTMLAnchorElement>,
    enabled: boolean,
  ) {
    if (pending || !enabled) {
      event.preventDefault();
      return;
    }
    if (
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey ||
      event.button !== 0
    )
      return;
    event.preventDefault();
    const href = event.currentTarget.href;
    if (!startBusy()) return;
    try {
      await finishAcquisition();
      window.location.assign(href);
    } catch {
      setError("Could not start sign-in.");
      stopBusy();
    }
  }
  const entry = stage === "start" || stage === "password";
  const newPassword = ["signup", "verify", "reset"].includes(stage);
  const needsBot =
    entry &&
    !!config?.turnstileSiteKey &&
    !(stage === "start" && !create && !reset);
  const emailCode = ["signup", "verify", "reset"].includes(stage);
  if (stage === "reset-complete")
    return (
      <div role="status" className="grid gap-4 text-sm">
        <p>
          Your password has been changed and previous sessions have been signed
          out.
        </p>
        <Link className="underline" href="/sign-in">
          Return to sign in
        </Link>
      </div>
    );
  return (
    <MotionHeight className="text-[14px]">
      {!reset && entry ? (
        <>
          <div className="grid gap-2">
            {(["github", "google"] as const).map((provider) => {
              const enabled =
                providers?.providers.some(
                  (item) => item.id === provider && item.enabled,
                ) ?? false;
              const query = new URLSearchParams({ mode, next: destination });
              return (
                <a
                  key={provider}
                  href={
                    enabled
                      ? `/api/canter/auth/oauth/${provider}?${query}`
                      : undefined
                  }
                  onClick={(event) => void startProvider(event, enabled)}
                  aria-disabled={pending || !enabled}
                  tabIndex={pending || !enabled ? -1 : 0}
                  className="relative flex h-11 items-center justify-center rounded-lg border border-[#363636] bg-[#222] hover:bg-[#2a2a2a] focus-visible:outline-2 focus-visible:outline-[#4aaaf0] aria-disabled:opacity-50"
                >
                  <span className="absolute left-3">
                    <ProviderIcon provider={provider} />
                  </span>
                  Continue with {provider === "github" ? "GitHub" : "Google"}
                </a>
              );
            })}
            {!create && config?.passkeys ? (
              <button
                type="button"
                disabled={pending}
                onClick={() => void passkeyLogin()}
                className="h-11 rounded-lg border border-[#363636] bg-[#222] hover:bg-[#2a2a2a] disabled:opacity-50"
              >
                Sign in with a passkey
              </button>
            ) : null}
          </div>
          <div className="my-6 flex items-center gap-3 text-[10px] text-[#aaa]">
            <span className="h-px flex-1 bg-[#252525]" />
            OR
            <span className="h-px flex-1 bg-[#252525]" />
          </div>
        </>
      ) : null}
      {loadError ? (
        <p role="alert" className="mb-4 text-[#f2b4b4]">
          Canter couldn’t be reached.{" "}
          <button
            type="button"
            className="underline"
            onClick={() => setAttempt((value) => value + 1)}
          >
            Try again
          </button>
        </p>
      ) : null}
      {(create || reset) && config && !config.email ? (
        <p role="status" className="mb-4 text-[#ccc]">
          Email verification is temporarily unavailable. Please try again later.
        </p>
      ) : null}
      {emailCode ? (
        <p className="mb-4 leading-6 text-[#ccc]">
          Enter the code sent to {email}.{" "}
          {stage === "verify"
            ? "To secure your existing account, choose a new password after verifying your email."
            : "The code expires in 10 minutes."}
        </p>
      ) : null}
      {stage === "mfa" ? (
        <h2 className="mb-4 text-lg">Verify it’s you</h2>
      ) : null}
      <form
        ref={formRef}
        onSubmit={submit}
        className="grid gap-3"
        aria-busy={pending}
      >
        {entry ? (
          <>
            <label htmlFor={emailId} className="text-[#bcbcbc]">
              Email address
            </label>
            <input
              id={emailId}
              name="email"
              type="email"
              autoComplete={create || reset ? "email" : "username webauthn"}
              autoCapitalize="none"
              spellCheck={false}
              placeholder="you@example.com"
              required
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              readOnly={pending}
              className={inputClass}
            />
          </>
        ) : null}
        {emailCode || stage === "mfa" ? (
          <>
            <label htmlFor={codeId}>
              {stage === "mfa"
                ? "Authenticator or recovery code"
                : "Email verification code"}
            </label>
            <input
              id={codeId}
              name="code"
              type="text"
              autoComplete="one-time-code"
              inputMode={stage === "mfa" ? "text" : "numeric"}
              pattern={stage === "mfa" ? undefined : "[0-9]{6}"}
              maxLength={stage === "mfa" ? 30 : 6}
              required
              readOnly={pending}
              className={inputClass}
              data-motion="fade"
            />
          </>
        ) : null}
        {stage === "password" || newPassword ? (
          <>
            <label htmlFor={passwordId}>
              {newPassword ? "New password" : "Password"}
            </label>
            <div className="relative" data-motion="fade">
              <input
                id={passwordId}
                name="password"
                type={showPassword ? "text" : "password"}
                autoComplete={newPassword ? "new-password" : "current-password"}
                required
                maxLength={2048}
                readOnly={pending}
                className={`${inputClass} pr-16`}
              />
              <button
                type="button"
                className="absolute right-1 top-1 h-9 rounded-md px-2 text-[#bcbcbc] hover:bg-[#333]"
                aria-label={showPassword ? "Hide password" : "Show password"}
                aria-pressed={showPassword}
                onClick={() => setShowPassword(!showPassword)}
              >
                <MorphLabel text={showPassword ? "Hide" : "Show"} />
              </button>
            </div>
            {newPassword ? (
              <p className="text-xs text-[#aaa]">
                Use at least 15 characters. A long, unique passphrase works
                well.
              </p>
            ) : (
              <Link
                href="/reset-password"
                className="text-xs text-[#4aaaf0] hover:underline"
              >
                Forgot your password?
              </Link>
            )}
          </>
        ) : null}
        {stage === "signup" && providers?.requireInvite ? (
          <label>
            Invitation code
            <input name="invite" required className={`${inputClass} mt-2`} />
          </label>
        ) : null}
        {stage === "reset" || stage === "verify" ? (
          <>
            <label htmlFor={factorId}>
              Authenticator or recovery code{" "}
              <span className="text-xs text-[#aaa]">if enabled</span>
            </label>
            <input
              id={factorId}
              name="factor"
              autoComplete="off"
              maxLength={30}
              className={inputClass}
            />
            <p className="text-xs text-[#aaa]">
              Resetting your password keeps two-factor authentication enabled.
            </p>
          </>
        ) : null}
        {needsBot ? (
          <AuthBotCheck
            siteKey={config!.turnstileSiteKey}
            onToken={setBotToken}
            attempt={botAttempt}
          />
        ) : null}
        {accountDeleted ? <p role="status" className="py-2 leading-5 text-[#b8c9bd]">Your account has been deleted and all sessions have been signed out.</p> : null}
        {error ? (
          <p role="alert" className="py-2 leading-5 text-[#f2a5a5]">
            {error}
          </p>
        ) : null}
        <button
          disabled={
            pending ||
            !config ||
            ((create || reset) && !config.email) ||
            (needsBot && !botToken)
          }
          style={{ color: "#111" }}
          className="h-11 rounded-lg border border-[#d1d1d1] bg-[#e5e5e5] hover:bg-white focus-visible:outline-2 focus-visible:outline-[#4aaaf0] disabled:opacity-60"
        >
          <MorphLabel shimmer={pending} text={pending
            ? "Please wait…"
            : stage === "start"
              ? reset || create
                ? "Send verification code"
                : "Continue with email"
              : stage === "signup"
                ? "Create account"
                : stage === "reset"
                  ? "Reset password"
                  : stage === "mfa" || stage === "verify"
                    ? "Verify and continue"
                    : "Log in"} />
        </button>
      </form>
      {emailCode ? (
        <div className="mt-4 text-xs text-[#aaa]">
          <button
            type="button"
            disabled={pending || cooldown > 0}
            className="underline disabled:opacity-50"
            onClick={() => {
              setStage("start");
              setError("");
            }}
          >
            {cooldown > 0
              ? `Request another code in ${cooldown}s`
              : "Request a new code or change email"}
          </button>
        </div>
      ) : null}
      {stage === "mfa" ? (
        <p className="mt-4 text-xs text-[#aaa]">
          Lost your authenticator? Use a saved recovery code, or return to sign
          in with a passkey.
        </p>
      ) : null}
      <p className="mt-6 text-center text-[#aaa]">
        {create
          ? "Already have an account? "
          : reset || !entry
            ? ""
            : "Don't have an account? "}
        <Link
          href={`${create || reset || !entry ? "/sign-in" : "/create-account"}${next ? `?next=${encodeURIComponent(destination)}` : ""}`}
          style={{ color: "#4aaaf0" }}
          className="hover:underline"
          onClick={() => {
            setStage("start");
            setError("");
          }}
        >
          {create || reset || !entry ? "Back to sign in" : "Sign up"}
        </Link>
      </p>
    </MotionHeight>
  );
}
