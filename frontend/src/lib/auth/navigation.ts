export function loginDestination(value: string | null) {
  if (!value?.startsWith("/") || value.startsWith("//") || /[\\\u0000-\u001f\u007f]/.test(value))
    return "/";
  const destination = new URL(value, "https://workspace.invalid");
  if (
    destination.origin !== "https://workspace.invalid" ||
    destination.pathname === "/login" ||
    destination.pathname.startsWith("/login/") ||
    destination.pathname === "/api" ||
    destination.pathname.startsWith("/api/")
  )
    return "/";
  return `${destination.pathname}${destination.search}${destination.hash}`;
}
