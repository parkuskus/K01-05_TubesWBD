"use client";

import { useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ApiError, createApiClient } from "@/lib/api/client";
import type { AuthSession, LoginInput, SessionUser } from "@/lib/auth/types";

type SessionState =
  | { status: "loading" | "anonymous" | "error"; user: null }
  | { status: "authenticated"; user: SessionUser };

type AuthContextValue = SessionState & {
  api: ReturnType<typeof createApiClient>;
  refreshSession: (signal?: AbortSignal) => Promise<void>;
  login: (input: LoginInput) => Promise<void>;
  logout: () => Promise<void>;
};

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [api] = useState(() => createApiClient());
  const [session, setSession] = useState<SessionState>({ status: "loading", user: null });
  const generation = useRef(0);
  const mutationPending = useRef(false);
  const userID = useRef<string | null>(null);

  const clearSession = useCallback(() => {
    generation.current++;
    api.setCsrfToken(null);
    userID.current = null;
    queryClient.clear();
    setSession({ status: "anonymous", user: null });
  }, [api, queryClient]);

  const refreshSession = useCallback(
    async (signal?: AbortSignal) => {
      if (mutationPending.current) return;
      const current = ++generation.current;
      try {
        const csrf = await api.request<{ csrf_token: string }>("/csrf", { signal });
        if (signal?.aborted || current !== generation.current) return;
        if (!csrf?.csrf_token) throw new Error("Invalid CSRF response");
        api.setCsrfToken(csrf.csrf_token);
        const data = await api.request<AuthSession>("/auth/me", { signal });
        if (signal?.aborted || current !== generation.current) return;
        if (!data?.user) throw new Error("Invalid session response");
        if (userID.current !== data.user.id) queryClient.clear();
        userID.current = data.user.id;
        setSession({ status: "authenticated", user: data.user });
      } catch (error) {
        if (signal?.aborted || current !== generation.current) return;
        if (error instanceof ApiError && error.status === 401) {
          clearSession();
        } else {
          api.setCsrfToken(null);
          userID.current = null;
          queryClient.clear();
          setSession({ status: "error", user: null });
        }
      }
    },
    [api, clearSession, queryClient],
  );

  useEffect(() => {
    const controller = new AbortController();
    api.setUnauthorizedHandler((path) => {
      if (path !== "/auth/login" && path !== "/auth/me") clearSession();
    });
    queueMicrotask(() => void refreshSession(controller.signal));
    const revalidate = () => {
      if (document.visibilityState === "visible") void refreshSession(controller.signal);
    };
    window.addEventListener("focus", revalidate);
    window.addEventListener("pageshow", revalidate);
    document.addEventListener("visibilitychange", revalidate);
    return () => {
      controller.abort();
      api.setUnauthorizedHandler(null);
      window.removeEventListener("focus", revalidate);
      window.removeEventListener("pageshow", revalidate);
      document.removeEventListener("visibilitychange", revalidate);
    };
  }, [api, clearSession, refreshSession]);

  async function login(input: LoginInput) {
    if (mutationPending.current) return;
    mutationPending.current = true;
    const current = ++generation.current;
    try {
      const csrf = await api.request<{ csrf_token: string }>("/csrf");
      if (current !== generation.current) return;
      if (!csrf?.csrf_token) throw new Error("Invalid CSRF response");
      api.setCsrfToken(csrf.csrf_token);
      const data = await api.request<AuthSession>("/auth/login", {
        method: "POST",
        body: JSON.stringify(input),
      });
      if (current !== generation.current) return;
      if (!data?.user) throw new Error("Invalid session response");
      queryClient.clear();
      userID.current = data.user.id;
      setSession({ status: "authenticated", user: data.user });
    } finally {
      mutationPending.current = false;
    }
  }

  async function logout() {
    if (mutationPending.current) return;
    mutationPending.current = true;
    generation.current++;
    try {
      await api.request("/auth/logout", { method: "POST" });
      clearSession();
    } finally {
      mutationPending.current = false;
    }
  }

  return (
    <AuthContext.Provider value={{ ...session, api, refreshSession, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const auth = useContext(AuthContext);
  if (!auth) throw new Error("AuthProvider is required.");
  return auth;
}
