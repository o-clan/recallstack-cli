import { injectTraceHeaders, withCliHttpSpan } from "./tracing.js";
import { CLI_CLIENT_ID } from "./version.js";

export class HttpError extends Error {
  readonly status: number;
  readonly payload: unknown;
  readonly code: string | undefined;

  constructor(status: number, payload: unknown, code?: string) {
    super(`HTTP ${status}: ${JSON.stringify(payload)}`);
    this.name = "HttpError";
    this.status = status;
    this.payload = payload;
    this.code = code;
  }
}

function isLocalHostname(hostname: string): boolean {
  return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "::1";
}

function getLocalTransportHint(url: URL, code: string | undefined): string | undefined {
  if (url.protocol !== "https:" || !isLocalHostname(url.hostname)) {
    return undefined;
  }

  if (code === "ERR_SSL_WRONG_VERSION_NUMBER") {
    return `Local Recallstack dev listens on http by default. Try \`recallstack config base-url http://${url.host}\` or enable HTTPS for your local API.`;
  }

  if (code === "DEPTH_ZERO_SELF_SIGNED_CERT" || code === "SELF_SIGNED_CERT_IN_CHAIN" || code === "ERR_TLS_CERT_ALTNAME_INVALID") {
    return `Local HTTPS certificate validation failed. Trust your local certificate, use a hostname covered by the cert, or switch to \`http://${url.host}\` for the default dev stack.`;
  }

  return undefined;
}

function wrapTransportError(targetUrl: string, error: unknown): Error {
  const baseMessage = error instanceof Error ? error.message : String(error);
  const maybeCause = error instanceof Error && typeof error.cause === "object" && error.cause !== null
    ? error.cause as { code?: unknown }
    : undefined;
  const causeCode = typeof maybeCause?.code === "string" ? maybeCause.code : undefined;

  try {
    const url = new URL(targetUrl);
    const hint = getLocalTransportHint(url, causeCode);
    const suffix = hint ? ` ${hint}` : "";
    return new Error(`Request to ${targetUrl} failed: ${baseMessage}.${suffix}`, { cause: error });
  } catch {
    return new Error(`Request failed: ${baseMessage}.`, { cause: error });
  }
}

export async function http<T>(baseUrl: string, path: string, options: {
  method?: string;
  body?: unknown;
  token?: string;
  workspaceId?: string;
  disableTracing?: boolean;
} = {}): Promise<T> {
  const headers: Record<string, string> = {
    "content-type": "application/json",
    "user-agent": CLI_CLIENT_ID,
    "x-recallstack-client": CLI_CLIENT_ID,
  };

  if (options.token) {
    headers.authorization = `Bearer ${options.token}`;
  }
  if (options.workspaceId) {
    headers["x-workspace-id"] = options.workspaceId;
  }

  const execute = async () => fetch(`${baseUrl}${path}`, {
    method: options.method || "GET",
    headers: injectTraceHeaders({ ...headers }),
    body: options.body ? JSON.stringify(options.body) : undefined,
  });
  const targetUrl = `${baseUrl}${path}`;
  let response: Response;
  try {
    response = options.disableTracing || path.startsWith("/v1/telemetry/")
      ? await execute()
      : await withCliHttpSpan({
        method: options.method || "GET",
        url: targetUrl,
        attributes: {
          "workspace.id": options.workspaceId,
        },
      }, async () => execute());
  } catch (error) {
    throw wrapTransportError(targetUrl, error);
  }

  const payload = (await response.json().catch(() => ({}))) as T & {
    error?: {
      code?: string;
    };
    code?: string;
  };
  if (!response.ok) {
    const code = payload?.error?.code ?? payload?.code;
    if (response.status === 403 && code === "LEGAL_CONSENT_REQUIRED") {
      const consentUrl = new URL("/consent?next=%2Fdashboard", baseUrl).toString();
      throw new Error(`Legal consent required. Open ${consentUrl}, accept Terms and Privacy, then retry.`);
    }
    throw new HttpError(response.status, payload, code);
  }

  return payload;
}
