/**
 * The consent page's view of a third-party app's authorize request (RFC
 * 6749 s4.1.1): what the app put in the link, what it lacks, and how the
 * page describes where the browser goes afterwards.
 */
export type OAuthAuthorizeSearch = {
  client_id?: string;
  redirect_uri?: string;
  scope?: string;
  state?: string;
  code_challenge?: string;
  code_challenge_method?: string;
};

const KEYS = ["client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"] as const;
const REQUIRED = ["client_id", "redirect_uri", "code_challenge", "code_challenge_method"] as const;

/** The authorize parameters in a location's search, strings only. */
export function parseAuthorizeSearch(s: Record<string, unknown>): OAuthAuthorizeSearch {
  const out: OAuthAuthorizeSearch = {};
  for (const k of KEYS) {
    const v = s[k];
    if (typeof v === "string" && v !== "") out[k] = v;
  }
  return out;
}

/** The required parameters the app's link lacks; empty when it is complete. */
export function missingAuthorizeParams(s: OAuthAuthorizeSearch): string[] {
  return REQUIRED.filter((k) => !s[k]);
}

/** The host the browser is sent back to, for the consent text. */
export function redirectHost(uri: string): string {
  try {
    return new URL(uri).host;
  } catch {
    return uri;
  }
}

/**
 * Whether the page may send the browser to a redirect the server answered
 * with: an absolute http(s) URL, nothing else (no javascript: or data:).
 */
export function isFollowableRedirect(url: string): boolean {
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:";
  } catch {
    return false;
  }
}
