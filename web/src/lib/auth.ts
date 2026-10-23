// auth.ts — access-key authentication for non-loopback clients.
//
// The browser authenticates via the octo_access_key cookie: loopback visits
// never see a 401 (the server exempts loopback peers — see internal/server/
// auth.go), so the prompt only appears when the server is reached over the
// network. A ?access_key= query parameter (from the URL the server prints on
// startup) is adopted into storage and stripped from the address bar; the
// cookie then rides every same-origin fetch and the WebSocket handshake.
//
// The cookie alone cannot be relied on, though: a Secure cookie saved on an
// earlier https visit to the same host is neither sent over http nor
// overwritable from an insecure page (the browser silently ignores the
// re-set — and document.cookie hides it, so we cannot even tell). The key in
// localStorage therefore rides as an X-Access-Key header on gated fetches
// (see authFetch) and as ?access_key= on the WebSocket URL; the server
// already accepts both forms, and the loopback exemption covers plaintext
// visits anyway, so the header never widens what a mixed-scheme cookie leak
// could reach.

import { writable } from "svelte/store";

const COOKIE_NAME = "octo_access_key";
const STORAGE_KEY = "octo_access_key";
// A gated endpoint (unlike /api/health and /api/version, which are exempt) so a
// 401 here means "key needed", not "wrong path".
const PROBE_ENDPOINT = "/api/sessions?limit=1";
const MAX_PROMPT_TRIES = 3;

// Drives the AuthGate overlay. null = no prompt showing. `retry` is true once
// the user has already submitted a wrong key this session.
export const authPrompt = writable<{ retry: boolean } | null>(null);

let resolvePrompt: ((key: string | null) => void) | null = null;

// Called by the AuthGate overlay: a key (submit) or null (cancel) resolves the
// promise the check loop is awaiting.
export function submitAuthKey(key: string | null): void {
  authPrompt.set(null);
  const r = resolvePrompt;
  resolvePrompt = null;
  r?.(key);
}

function askUserForKey(retry: boolean): Promise<string | null> {
  return new Promise((resolve) => {
    resolvePrompt = resolve;
    authPrompt.set({ retry });
  });
}

function setCookie(key: string): void {
  const secure = location.protocol === "https:" ? "; Secure" : "";
  // encodeURIComponent matches the server's url.PathUnescape on read.
  document.cookie = `${COOKIE_NAME}=${encodeURIComponent(key)}; path=/; SameSite=Strict${secure}`;
}

function clearCookie(): void {
  document.cookie = `${COOKIE_NAME}=; path=/; max-age=0; SameSite=Strict`;
}

// storedKey is the localStorage mirror of the last accepted key. The header
// and query-parameter fallbacks read it instead of document.cookie, since
// that is exactly the place a set-cookie attempt cannot reach (see the file
// comment): a Secure cookie from an https visit shadows any http rewrite.
export function storedKey(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

// authHeaders returns the fallback header for a stored key. Raw key bytes —
// the server reads X-Access-Key verbatim while the cookie goes through
// PathUnescape, so encoding here would corrupt keys containing % or +.
export function authHeaders(): Record<string, string> {
  const key = storedKey();
  return key ? { "X-Access-Key": key } : {};
}

// mergeAuthHeaders normalises the caller's headers into a Headers instance
// before layering the fallback on top. A plain object spread drops a Headers
// instance (its entries are internal slots, not own properties) and mangles
// the array-of-pairs form, so builds that pass either would silently lose
// their Content-Type or the key.
export function mergeAuthHeaders(init?: HeadersInit): Headers {
  const h = new Headers(init);
  if (!h.has("X-Access-Key")) {
    const key = storedKey();
    if (key) h.set("X-Access-Key", key);
  }
  return h;
}

// authFetch is fetch plus the header fallback. Only flagged call sites use
// it; cookie auth still handles the normal browser case, so unchanged
// callers (non-gated or tokenized URLs) keep their original semantics.
export function authFetch(path: string, init?: RequestInit): Promise<Response> {
  return fetch(path, { ...init, headers: mergeAuthHeaders(init?.headers) });
}

// wsQuery appends the key as ?access_key= for the WebSocket handshake — a
// browser WebSocket can set neither headers nor cookies on demand, and the
// Secure-cookie wedge above hits ws harder than any fetch. Appending is
// safe even when the cookie already authenticates (the server treats the
// query key as just another presented credential) and is skipped when
// nothing is stored, which keeps the loopback exemption path byte-identical.
export function wsQuery(path: string): string {
  const key = storedKey();
  if (!key) return path;
  return `${path}?access_key=${encodeURIComponent(key)}`;
}

// Adopt a bootstrap-link key, then strip it so it doesn't linger in the address
// bar or browser history.
function adoptQueryKey(): void {
  const params = new URLSearchParams(location.search);
  const key = params.get("access_key");
  if (!key) return;
  localStorage.setItem(STORAGE_KEY, key);
  setCookie(key);
  params.delete("access_key");
  const qs = params.toString();
  history.replaceState(null, "", location.pathname + (qs ? `?${qs}` : "") + location.hash);
}

async function probe(presentedKey?: string): Promise<"ok" | "unauthorized" | "other"> {
  try {
    // The header rides alongside the cookie: whichever one survives
    // (cookie on a fresh page, header when a Secure cookie wedges it out)
    // authenticates, so the probe mirrors every other gated call.
    const r = await fetch(PROBE_ENDPOINT, {
      headers: presentedKey
        ? { "X-Access-Key": presentedKey }
        : authHeaders(),
    });
    if (r.ok) return "ok";
    if (r.status === 401) return "unauthorized";
    return "other";
  } catch {
    return "other";
  }
}

let checkPromise: Promise<boolean> | null = null;

// Resolve auth before the app makes any gated call. Returns false only when the
// server demands a key and the user couldn't provide a valid one.
export function checkAuth(): Promise<boolean> {
  if (!checkPromise) checkPromise = doCheck();
  return checkPromise;
}

// Shared retry loop: prompt for a key up to MAX_PROMPT_TRIES times, probing
// after each submission. Used both at boot (doCheck) and by a live client
// that discovers mid-session its key was revoked (reauth).
async function promptAndVerify(): Promise<boolean> {
  for (let i = 0; i < MAX_PROMPT_TRIES; i++) {
    const key = await askUserForKey(i > 0);
    if (!key) break; // user cancelled
    setCookie(key);
    if ((await probe(key)) === "ok") {
      localStorage.setItem(STORAGE_KEY, key);
      return true;
    }
  }
  clearCookie();
  localStorage.removeItem(STORAGE_KEY);
  return false;
}

async function doCheck(): Promise<boolean> {
  adoptQueryKey();
  // Re-seed the cookie from storage in case it was cleared.
  const stored = localStorage.getItem(STORAGE_KEY);
  if (stored) setCookie(stored);

  // The probe presents the stored key as a header too: on a wedged page
  // (Secure cookie from an https visit can neither be read nor overwritten
  // over http) the cookie never reaches the server, and the header keeps
  // boot from prompting for a key the storage already holds.
  const status = await probe(stored ?? undefined);
  // ok — or a server/network error, which is not an auth failure and gets
  // surfaced by whichever real call hits it; don't block boot on it.
  if (status !== "unauthorized") return true;
  return promptAndVerify();
}

// Confirms whether the *current* cookie is rejected right now — a fresh,
// uncached probe (unlike checkAuth(), which memoizes its result for the
// process lifetime). Used by the WebSocket reconnect loop to tell a revoked
// key apart from an ordinary network drop: only a real 401 justifies giving
// up on backoff and re-prompting; a fetch failure here just means the network
// is down, which the caller should retry as usual.
// Exported so a fresh boot burns exactly one prompt loop even with a
// secure-cookie wedge; the header fallback keeps the verify honest.
export async function isUnauthorized(): Promise<boolean> {
  return (await probe(storedKey() ?? undefined)) === "unauthorized";
}

// Re-probes and, if still rejected, re-runs the same prompt-and-verify loop
// checkAuth() uses at boot — for a live client that already passed the boot
// check but later finds its key no longer works (e.g. the operator rotated
// it). Not memoized: each call re-probes, so it reflects the current cookie.
export async function reauth(): Promise<boolean> {
  const status = await probe(storedKey() ?? undefined);
  if (status !== "unauthorized") return true;
  return promptAndVerify();
}
