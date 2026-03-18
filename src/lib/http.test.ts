import assert from "node:assert/strict";
import test from "node:test";

import { HttpError, http } from "./http.js";

test("http wraps localhost https TLS mismatch failures with a dev-stack hint", async () => {
  const originalFetch = globalThis.fetch;
  const cause = Object.assign(new Error("ssl wrong version number"), {
    code: "ERR_SSL_WRONG_VERSION_NUMBER",
  });

  globalThis.fetch = (async () => {
    throw new TypeError("fetch failed", { cause });
  }) as typeof fetch;

  try {
    await assert.rejects(
      () => http("https://localhost:35000", "/v1/auth/cli/code/exchange", { disableTracing: true }),
      (error: unknown) => {
        assert.ok(error instanceof Error);
        assert.match(error.message, /Request to https:\/\/localhost:35000\/v1\/auth\/cli\/code\/exchange failed: fetch failed\./);
        assert.match(error.message, /Local Recallstack dev listens on http by default\./);
        assert.match(error.message, /recallstack config base-url http:\/\/localhost:35000/);
        return true;
      },
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("http still throws HttpError for non-ok API responses", async () => {
  const originalFetch = globalThis.fetch;

  globalThis.fetch = (async () => new Response(
    JSON.stringify({ error: { code: "UNAUTHORIZED" } }),
    {
      status: 401,
      headers: {
        "content-type": "application/json",
      },
    },
  )) as typeof fetch;

  try {
    await assert.rejects(
      () => http("http://localhost:35000", "/v1/auth/me", { disableTracing: true }),
      (error: unknown) => {
        assert.ok(error instanceof HttpError);
        assert.equal(error.status, 401);
        assert.equal(error.code, "UNAUTHORIZED");
        return true;
      },
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});
