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

test("malformed URL stays undefined", () => {
  expect(detectAuthFailure("not a url", "stale")).toBeUndefined();
});
