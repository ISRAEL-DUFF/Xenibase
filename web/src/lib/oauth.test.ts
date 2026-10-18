import { describe, expect, it } from "vitest";
import { isFollowableRedirect, missingAuthorizeParams, parseAuthorizeSearch, redirectHost } from "./oauth";

describe("oauth consent helpers", () => {
  it("keeps only the known string parameters", () => {
    expect(
      parseAuthorizeSearch({
        client_id: "xbapp_1",
        redirect_uri: "https://app.taskiem.test/cb",
        scope: "read write",
        state: "",
        code_challenge: 42,
        code_challenge_method: "S256",
        extra: "x",
      }),
    ).toEqual({ client_id: "xbapp_1", redirect_uri: "https://app.taskiem.test/cb", scope: "read write", code_challenge_method: "S256" });
  });

  it("names what a link lacks", () => {
    expect(missingAuthorizeParams({})).toEqual(["client_id", "redirect_uri", "code_challenge", "code_challenge_method"]);
    expect(missingAuthorizeParams({ client_id: "a", redirect_uri: "b", code_challenge: "c", code_challenge_method: "S256" })).toEqual([]);
    expect(missingAuthorizeParams({ client_id: "a", redirect_uri: "b", scope: "read" })).toEqual(["code_challenge", "code_challenge_method"]);
  });

  it("describes the redirect by its host", () => {
    expect(redirectHost("https://app.taskiem.test/connections/xenibase/callback?src=xb")).toBe("app.taskiem.test");
    expect(redirectHost("http://localhost:3000/cb")).toBe("localhost:3000");
    expect(redirectHost("not a url")).toBe("not a url");
  });

  it("follows only http(s) redirects", () => {
    expect(isFollowableRedirect("https://app.taskiem.test/cb?code=x&state=y")).toBe(true);
    expect(isFollowableRedirect("http://localhost:3000/cb?error=access_denied")).toBe(true);
    expect(isFollowableRedirect("javascript:alert(1)")).toBe(false);
    expect(isFollowableRedirect("data:text/html,hi")).toBe(false);
    expect(isFollowableRedirect("/relative")).toBe(false);
  });
});
