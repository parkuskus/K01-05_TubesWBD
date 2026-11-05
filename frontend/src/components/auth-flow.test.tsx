import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AuthProvider, useAuth } from "@/components/auth-provider";
import { LoginScreen } from "@/components/login-screen";
import { ProtectedWorkspace } from "@/components/protected-workspace";

const navigation = vi.hoisted(() => ({
  replace: vi.fn(),
  next: "/activity",
  pathname: "/activity",
}));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: navigation.replace }),
  useSearchParams: () => new URLSearchParams({ next: navigation.next }),
  usePathname: () => navigation.pathname,
}));

const account = { id: "42", username: "Emilio", is_platform_admin: true };
let currentAccount = account;
let authenticated = false;
let unavailable = false;
let rejectedLogin = false;
let calls: { path: string; init: RequestInit }[] = [];

beforeEach(() => {
  navigation.replace.mockClear();
  navigation.next = "/activity";
  authenticated = false;
  unavailable = false;
  rejectedLogin = false;
  calls = [];
  currentAccount = account;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init: RequestInit) => {
      calls.push({ path, init });
      if (unavailable) throw new TypeError("offline");
      if (path.endsWith("/csrf")) return Response.json({ data: { csrf_token: "test-csrf" } });
      if (path.endsWith("/auth/login")) {
        if (rejectedLogin)
          return Response.json(
            { error: { code: "INVALID_CREDENTIALS", message: "private-dsn" } },
            { status: 401 },
          );
        authenticated = true;
        return Response.json({ data: { user: currentAccount } });
      }
      if (path.endsWith("/auth/logout")) {
        authenticated = false;
        return new Response(null, { status: 204 });
      }
      if (path.endsWith("/auth/me") && authenticated)
        return Response.json({ data: { user: currentAccount } });
      return Response.json(
        { error: { code: "UNAUTHENTICATED", message: "Masuk kembali." } },
        { status: 401 },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderAuth(children: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <AuthProvider>{children}</AuthProvider>
    </QueryClientProvider>,
  );
  return client;
}

function SessionProbe() {
  const auth = useAuth();
  return (
    <>
      <p role="status">{auth.status}</p>
      {auth.user && <p>{auth.user.username}</p>}
      <button onClick={() => void auth.logout()}>Logout</button>
      <button onClick={() => void auth.api.request("/private").catch(() => undefined)}>
        Private request
      </button>
    </>
  );
}

it("bootstraps an existing cookie session and clears cached private data on logout", async () => {
  authenticated = true;
  const client = renderAuth(<SessionProbe />);
  await screen.findByText("authenticated");
  expect(screen.getByText("Emilio")).toBeTruthy();
  client.setQueryData(["private", "databases"], ["private-record"]);
  await userEvent.setup().click(screen.getByRole("button", { name: "Logout" }));
  await screen.findByText("anonymous");
  expect(client.getQueryData(["private", "databases"])).toBeUndefined();
  const request = calls.find(({ path }) => path.endsWith("/auth/logout"));
  expect(new Headers(request?.init.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(request?.init.credentials).toBe("same-origin");
});

it("expires the session globally when an authenticated feature returns 401", async () => {
  authenticated = true;
  const client = renderAuth(<SessionProbe />);
  await screen.findByText("authenticated");
  client.setQueryData(["private"], "secret");
  await userEvent.setup().click(screen.getByRole("button", { name: "Private request" }));
  await screen.findByText("anonymous");
  expect(client.getQueryData(["private"])).toBeUndefined();
  expect(screen.queryByText("Emilio")).toBeNull();
});

it("rechecks a revoked session when the window regains focus", async () => {
  authenticated = true;
  renderAuth(<SessionProbe />);
  await screen.findByText("authenticated");
  authenticated = false;
  act(() => window.dispatchEvent(new Event("focus")));
  await screen.findByText("anonymous");
});

it("clears the previous account cache when a different session is discovered", async () => {
  authenticated = true;
  const client = renderAuth(<SessionProbe />);
  await screen.findByText("Emilio");
  client.setQueryData(["private"], "previous-account-data");
  currentAccount = { id: "43", username: "Other account", is_platform_admin: false };
  act(() => window.dispatchEvent(new Event("focus")));
  await screen.findByText("Other account");
  expect(client.getQueryData(["private"])).toBeUndefined();
});

it("submits the login through JSON with CSRF and restores the requested route", async () => {
  renderAuth(<LoginScreen />);
  const user = userEvent.setup();
  await user.type(await screen.findByRole("textbox", { name: "Username" }), "Emilio");
  await user.type(screen.getByLabelText("Password", { exact: true }), "correct-password");
  await user.click(screen.getByRole("button", { name: "Tampilkan password" }));
  expect((screen.getByLabelText("Password", { exact: true }) as HTMLInputElement).type).toBe(
    "text",
  );
  await user.click(screen.getByRole("button", { name: "Masuk" }));
  await waitFor(() => expect(navigation.replace).toHaveBeenCalledWith("/activity"));
  const request = calls.find(({ path }) => path.endsWith("/auth/login"));
  expect(JSON.parse(request?.init.body as string)).toEqual({
    username: "Emilio",
    password: "correct-password",
  });
  expect(new Headers(request?.init.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(new Headers(request?.init.headers).get("Content-Type")).toBe("application/json");
});

it("keeps failed login on the form, clears the password and hides raw backend errors", async () => {
  rejectedLogin = true;
  renderAuth(<LoginScreen />);
  const user = userEvent.setup();
  await user.type(await screen.findByRole("textbox", { name: "Username" }), "Emilio");
  const password = screen.getByLabelText("Password", { exact: true }) as HTMLInputElement;
  await user.type(password, "wrong-password");
  await user.click(screen.getByRole("button", { name: "Masuk" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Username atau password salah.");
  expect(password.value).toBe("");
  expect(screen.queryByText("private-dsn")).toBeNull();
  expect(navigation.replace).not.toHaveBeenCalled();
});

it("does not render protected content before an anonymous deep link redirects", async () => {
  renderAuth(
    <ProtectedWorkspace>
      <p>Protected content</p>
    </ProtectedWorkspace>,
  );
  expect(screen.queryByText("Protected content")).toBeNull();
  await waitFor(() => expect(navigation.replace).toHaveBeenCalledWith("/login?next=%2Factivity"));
  expect(screen.queryByText("Protected content")).toBeNull();
});

it("shows a recoverable service error instead of an anonymous login when the API is offline", async () => {
  unavailable = true;
  renderAuth(<LoginScreen />);
  await screen.findByRole("alert");
  expect(screen.queryByRole("textbox", { name: "Username" })).toBeNull();
  expect(navigation.replace).not.toHaveBeenCalled();
  unavailable = false;
  await userEvent.setup().click(screen.getByRole("button", { name: "Coba lagi" }));
  expect(await screen.findByRole("textbox", { name: "Username" })).toBeTruthy();
});
