"use client";

import { Eye, EyeOff, LoaderCircle } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { useAuth } from "@/components/auth-provider";
import { Brand } from "@/components/brand";
import { SessionState } from "@/components/session-state";
import { Button, buttonStyles } from "@/components/ui/button";
import { TextField } from "@/components/ui/primitives";
import { ApiError } from "@/lib/api/client";
import { loginDestination } from "@/lib/auth/navigation";
import { cx } from "@/lib/utils";

export function LoginScreen() {
  const { status, login } = useAuth();
  const router = useRouter();
  const searchParams = useSearchParams();
  const destination = loginDestination(searchParams.get("next"));
  const [pending, setPending] = useState(false);
  const [visible, setVisible] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [message, setMessage] = useState("");
  const formRef = useRef<HTMLFormElement>(null);

  useEffect(() => {
    if (status === "authenticated") router.replace(destination);
  }, [destination, router, status]);

  useEffect(() => {
    if (pending) return;
    const field = ["username", "password"].find((name) => errors[name]);
    if (field) formRef.current?.querySelector<HTMLInputElement>(`[name="${field}"]`)?.focus();
  }, [errors, pending]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    const form = event.currentTarget;
    const values = new FormData(form);
    const username = String(values.get("username") ?? "").trim();
    const password = String(values.get("password") ?? "");
    const nextErrors: Record<string, string> = {};
    if (!username || new TextEncoder().encode(username).length > 64)
      nextErrors.username = "Isi username yang valid, maksimal 64 byte.";
    if (!password || new TextEncoder().encode(password).length > 72)
      nextErrors.password = "Isi password yang valid, maksimal 72 byte.";
    setErrors(nextErrors);
    setMessage("");
    if (Object.keys(nextErrors).length) return;
    setPending(true);
    try {
      await login({ username, password });
      form.reset();
    } catch (error) {
      if (error instanceof ApiError && error.code === "INVALID_CREDENTIALS") {
        setMessage("Username atau password salah.");
      } else if (error instanceof ApiError && error.code === "CSRF_INVALID") {
        setMessage("Sesi formulir berakhir. Silakan coba masuk kembali.");
      } else if (error instanceof ApiError && error.status === 422) {
        const fieldErrors: Record<string, string> = {};
        for (const field of ["username", "password"]) {
          const value = error.fieldErrors[field];
          if (typeof value === "string" && value) fieldErrors[field] = value;
        }
        setErrors(fieldErrors);
        if (!Object.keys(fieldErrors).length) setMessage("Periksa isian login Anda.");
      } else {
        setMessage("Belum dapat masuk. Periksa koneksi Anda dan coba lagi.");
      }
      const passwordField = form.elements.namedItem("password");
      if (passwordField instanceof HTMLInputElement) passwordField.value = "";
    } finally {
      setPending(false);
      setVisible(false);
    }
  }

  if (status !== "anonymous") return <SessionState />;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center bg-ground px-4 py-12 sm:px-6">
      <div className="w-full max-w-sm">
        <div className="mb-10 flex justify-center">
          <Brand />
        </div>
        <section
          aria-labelledby="login-title"
          className="rounded-lg border border-line bg-surface p-6 sm:p-8"
        >
          <h1 id="login-title" className="text-2xl font-semibold tracking-[-0.035em]">
            Masuk ke workspace
          </h1>
          <p className="mt-2 text-sm leading-6 text-ink-secondary">
            Gunakan akun platform Anda untuk melanjutkan.
          </p>
          <form ref={formRef} noValidate onSubmit={submit} className="mt-8 space-y-5">
            <fieldset disabled={pending} className="min-w-0 space-y-5 disabled:opacity-60">
              <TextField
                id="login-username"
                name="username"
                label="Username"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                required
                error={errors.username}
              />
              <div className="grid min-w-0 gap-2">
                <label htmlFor="login-password" className="text-sm font-medium">
                  Password
                </label>
                <div className="relative">
                  <input
                    id="login-password"
                    name="password"
                    type={visible ? "text" : "password"}
                    autoComplete="current-password"
                    required
                    aria-invalid={errors.password ? true : undefined}
                    aria-describedby={errors.password ? "login-password-error" : undefined}
                    className={cx(
                      "min-h-11 w-full min-w-0 rounded-md border bg-surface pr-12 pl-3 text-base sm:text-sm",
                      errors.password ? "border-danger" : "border-control-border",
                    )}
                  />
                  <button
                    type="button"
                    aria-label={visible ? "Sembunyikan password" : "Tampilkan password"}
                    aria-pressed={visible}
                    onClick={() => setVisible((current) => !current)}
                    className={buttonStyles({
                      variant: "ghost",
                      size: "icon",
                      className: "absolute top-0 right-0",
                    })}
                  >
                    {visible ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
                  </button>
                </div>
                {errors.password && (
                  <p id="login-password-error" className="text-xs leading-relaxed text-danger">
                    {errors.password}
                  </p>
                )}
              </div>
            </fieldset>
            {message && (
              <p role="alert" className="text-sm leading-6 text-danger">
                {message}
              </p>
            )}
            <Button type="submit" className="w-full" disabled={pending}>
              {pending && (
                <LoaderCircle
                  className="animate-spin motion-reduce:animate-none"
                  aria-hidden="true"
                />
              )}
              {pending ? "Memproses…" : "Masuk"}
            </Button>
          </form>
        </section>
      </div>
    </main>
  );
}
