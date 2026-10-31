// safeReturnPath returns the path to go back to after the login, or "/" if it could leave
// the application. Browsers read "\" as "/", so "/\evil.com" would become "//evil.com", an
// address of another site: backslashes (also percent-encoded) and control characters are
// refused, and the path must resolve to this origin.
export function safeReturnPath(v: unknown, origin = window.location.origin): string {
  if (typeof v !== "string" || !v.startsWith("/") || v.startsWith("//")) return "/";
  let decoded: string;
  try {
    decoded = decodeURIComponent(v);
  } catch {
    return "/";
  }
  if (/[\\\u0000-\u001f\u007f]/.test(decoded) || decoded.startsWith("//")) return "/";
  try {
    if (new URL(v, origin).origin !== origin) return "/";
  } catch {
    return "/";
  }
  return v;
}
