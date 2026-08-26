// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// IdP failure-page heuristics: the host gate must dominate — publisher pages
// never classify no matter what their URL or title says.

import { expect, test } from "bun:test";

import { detectAuthFailure } from "../src/authfail";

test("OpenAthens stale assertion page classifies stale_sso", () => {
  expect(
    detectAuthFailure(
      "https://login.openathens.net/saml/2/sso/error",
      "OpenAthens — stale request",
    ),
  ).toBe("stale_sso");
});

test("Shibboleth SSO path with expired marker classifies stale_sso", () => {
  expect(
    detectAuthFailure("https://sso.example.edu/Shibboleth.sso/SAML2/POST", "Assertion expired"),
  ).toBe("stale_sso");
});

test("IdP profile error page without stale marker classifies auth_error", () => {
  expect(
    detectAuthFailure("https://idp.example.edu/idp/profile/SAML2/Redirect/SSO", "Access denied"),
  ).toBe("auth_error");
});

test("stale marker wins over generic error marker", () => {
  expect(
    detectAuthFailure(
      "https://login.openathens.net/error",
      "Error — your session has expired",
    ),
  ).toBe("stale_sso");
});

test("provider paywall page never classifies", () => {
  expect(
    detectAuthFailure("https://journals.sagepub.com/doi/10.1177/000", "Access denied"),
  ).toBeUndefined();
});

test("publisher URL containing the word error never classifies", () => {
  expect(
    detectAuthFailure("https://www.tandfonline.com/error/expired-session", "Error"),
  ).toBeUndefined();
});

test("IdP page with no failure markers stays undefined", () => {
  expect(
    detectAuthFailure("https://login.openathens.net/auth", "Sign in"),
  ).toBeUndefined();
});

test("Elsevier terminal authorization resume page classifies auth_error", () => {
  const resume =
    "https://id.elsevier.com/as/session-token/resume/as/authorization.ping?client_id=SDFE-v4";
  expect(detectAuthFailure(resume, "Sorry")).toBe("auth_error");
  // The same endpoint is the live OAuth continuation before Elsevier renders
  // the terminal page. URL shape alone must never abort that continuation.
  expect(detectAuthFailure(resume, undefined)).toBeUndefined();
  expect(detectAuthFailure(resume, "ScienceDirect")).toBeUndefined();
  // A publisher page with the same generic title is not identity machinery.
  expect(
    detectAuthFailure(
      "https://www.sciencedirect.com/science/article/pii/S0747563216303168",
      "Sorry",
    ),
  ).toBeUndefined();
});

test("malformed URL stays undefined", () => {
  expect(detectAuthFailure("not a url", "stale")).toBeUndefined();
});

// The reported real-world dead end. Its URL is byte-for-byte the URL of the
// WORKING login form — only the title distinguishes them — so a URL-only
// heuristic here would either miss every stale page or navigate the user away
// from a login form they are typing into. Both directions are pinned.
test("Shibboleth stale request is classified from the title alone", () => {
  const staleURL = "https://idp.une.edu.au/idp/profile/SAML2/Redirect/SSO?execution=e1s2";
  expect(
    detectAuthFailure(staleURL, "University of New England Login Service - Stale Request"),
  ).toBe("stale_sso");
  // Same URL, live login form: must never classify, or the recovery re-drive
  // would reload the page out from under a half-typed password.
  expect(detectAuthFailure(staleURL, "University of New England Login Service")).toBeUndefined();
  // No title yet (the update that races `complete`): stay silent, wait for it.
  expect(detectAuthFailure(staleURL, undefined)).toBeUndefined();
});
