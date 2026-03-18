import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  context,
  defaultTextMapSetter,
  propagation,
  SpanKind,
  SpanStatusCode,
  trace,
  type Attributes,
  type Context,
  type Span,
} from "@opentelemetry/api";
import { ExportResultCode, W3CTraceContextPropagator } from "@opentelemetry/core";
import { resourceFromAttributes } from "@opentelemetry/resources";
import {
  BatchSpanProcessor,
  type ReadableSpan,
  type SpanExporter,
} from "@opentelemetry/sdk-trace-base";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { getEffectiveBaseUrl, type RecallstackConfig } from "./config.js";

type TraceAttributeValue = string | number | boolean | null;

type TraceRelayPayload = {
  source: "cli";
  service_name: string;
  project_id?: string;
  client_name?: string | null;
  resource_attributes?: Record<string, TraceAttributeValue>;
  spans: Array<{
    trace_id: string;
    span_id: string;
    parent_span_id?: string | null;
    name: string;
    start_time_unix_nano: string;
    end_time_unix_nano?: string | null;
    status?: "ok" | "error";
    attributes?: Record<string, TraceAttributeValue>;
  }>;
};

function isTraceIdHex(value: string | null | undefined): value is string {
  return Boolean(value && /^[0-9a-f]{32}$/i.test(value));
}

function traceIdHexToUuid(traceId: string): string {
  const normalized = traceId.toLowerCase();
  if (!isTraceIdHex(normalized)) return traceId;
  return [
    normalized.slice(0, 8),
    normalized.slice(8, 12),
    normalized.slice(12, 16),
    normalized.slice(16, 20),
    normalized.slice(20),
  ].join("-");
}

function resolveCliClientId(): string {
  try {
    const currentDir = dirname(fileURLToPath(import.meta.url));
    const packagePath = resolve(currentDir, "../../package.json");
    const parsed = JSON.parse(readFileSync(packagePath, "utf8")) as { version?: unknown };
    const version = typeof parsed.version === "string" && parsed.version.trim().length > 0
      ? parsed.version.trim()
      : "0.0.0";
    return `recallstack-cli/${version}`;
  } catch {
    return "recallstack-cli/0.0.0";
  }
}

export const CLI_CLIENT_ID = resolveCliClientId();

let provider: NodeTracerProvider | null = null;
let initialized = false;

function hrTimeToUnixNano(input: readonly [number, number]): string {
  return (BigInt(input[0]) * 1_000_000_000n + BigInt(input[1])).toString();
}

function toScalarRecord(input: Record<string, unknown>): Record<string, TraceAttributeValue> {
  const out: Record<string, TraceAttributeValue> = {};
  for (const [key, value] of Object.entries(input)) {
    if (
      value === null
      || typeof value === "string"
      || typeof value === "number"
      || typeof value === "boolean"
    ) {
      out[key] = value;
    }
  }
  return out;
}

class CliTraceRelayExporter implements SpanExporter {
  constructor(private readonly config: RecallstackConfig) {}

  export(spans: ReadableSpan[], resultCallback: (result: { code: ExportResultCode; error?: Error }) => void): void {
    void this.send(spans)
      .then(() => resultCallback({ code: ExportResultCode.SUCCESS }))
      .catch((error) => resultCallback({
        code: ExportResultCode.FAILED,
        error: error instanceof Error ? error : new Error(String(error)),
      }));
  }

  private async send(spans: ReadableSpan[]): Promise<void> {
    if (!spans.length) return;
    if (!this.config.accessToken && !this.config.apiKey) return;

    const payload: TraceRelayPayload = {
      source: "cli",
      service_name: "recallstack-cli",
      client_name: CLI_CLIENT_ID,
      resource_attributes: {
        surface: "cli",
      },
      spans: spans.map((span) => ({
        trace_id: span.spanContext().traceId,
        span_id: span.spanContext().spanId,
        parent_span_id: span.parentSpanContext?.spanId || null,
        name: span.name,
        start_time_unix_nano: hrTimeToUnixNano(span.startTime),
        end_time_unix_nano: hrTimeToUnixNano(span.endTime),
        status: span.status.code === SpanStatusCode.ERROR ? "error" : "ok",
        attributes: {
          ...toScalarRecord(span.resource.attributes),
          ...toScalarRecord(span.attributes),
        },
      })),
    };

    const headers: Record<string, string> = {
      "content-type": "application/json",
      "x-recallstack-client": CLI_CLIENT_ID,
    };
    const token = this.config.accessToken || this.config.apiKey;
    if (token) {
      headers.authorization = `Bearer ${token}`;
    }
    if (this.config.activeWorkspaceId) {
      headers["x-workspace-id"] = this.config.activeWorkspaceId;
    }

    const response = await fetch(`${getEffectiveBaseUrl(this.config)}/v1/telemetry/traces`, {
      method: "POST",
      headers,
      body: JSON.stringify(payload),
    });

    if (!response.ok) {
      throw new Error(`Trace relay failed with status ${response.status}`);
    }
  }

  async shutdown(): Promise<void> {
    return undefined;
  }

  async forceFlush(): Promise<void> {
    return undefined;
  }
}

export function initializeCliTracing(config: RecallstackConfig): void {
  if (initialized) return;
  provider = new NodeTracerProvider({
    resource: resourceFromAttributes({
      "service.name": "recallstack-cli",
      surface: "cli",
      "client.name": CLI_CLIENT_ID,
    }),
    spanProcessors: [
      new BatchSpanProcessor(new CliTraceRelayExporter(config), {
        maxExportBatchSize: 100,
        maxQueueSize: 400,
        scheduledDelayMillis: 250,
        exportTimeoutMillis: 3000,
      }),
    ],
  });
  provider.register({
    propagator: new W3CTraceContextPropagator(),
  });
  initialized = true;
}

export async function flushCliTracing(): Promise<void> {
  if (!provider) return;
  await provider.forceFlush().catch(() => undefined);
}

export function getCliTracer() {
  return trace.getTracer("recallstack-cli");
}

function toAttributes(input: Record<string, TraceAttributeValue | undefined> | undefined): Attributes {
  const out: Attributes = {};
  if (!input) return out;
  for (const [key, value] of Object.entries(input)) {
    if (
      typeof value === "string"
      || typeof value === "number"
      || typeof value === "boolean"
    ) {
      out[key] = value;
    }
  }
  return out;
}

export async function withCliSpan<T>(
  config: RecallstackConfig,
  input: {
    name: string;
    kind?: SpanKind;
    attributes?: Record<string, TraceAttributeValue | undefined>;
  },
  fn: () => Promise<T>,
): Promise<T> {
  initializeCliTracing(config);
  const tracer = getCliTracer();
  return tracer.startActiveSpan(input.name, {
    kind: input.kind ?? SpanKind.INTERNAL,
    attributes: toAttributes(input.attributes),
  }, async (span) => {
    try {
      return await fn();
    } catch (error) {
      span.recordException(error instanceof Error ? error : new Error(String(error)));
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: error instanceof Error ? error.message : String(error),
      });
      throw error;
    } finally {
      span.end();
      await flushCliTracing();
    }
  });
}

export function injectTraceHeaders(headers: Record<string, string>): Record<string, string> {
  propagation.inject(context.active(), headers, defaultTextMapSetter);
  const activeSpan = trace.getSpan(context.active());
  if (activeSpan) {
    headers["x-trace-id"] = traceIdHexToUuid(activeSpan.spanContext().traceId);
  }
  return headers;
}

export async function withCliHttpSpan<T>(
  input: {
    method: string;
    url: string;
    attributes?: Record<string, TraceAttributeValue | undefined>;
  },
  fn: (span: Span, activeContext: Context) => Promise<T>,
): Promise<T> {
  const tracer = getCliTracer();
  return tracer.startActiveSpan(input.method.toUpperCase() + " " + input.url, {
    kind: SpanKind.CLIENT,
    attributes: toAttributes({
      "http.method": input.method.toUpperCase(),
      "http.url": input.url,
      ...input.attributes,
    }),
  }, async (span) => {
    const activeContext = context.active();
    try {
      const result = await fn(span, activeContext);
      span.setStatus({ code: SpanStatusCode.OK });
      return result;
    } catch (error) {
      span.recordException(error instanceof Error ? error : new Error(String(error)));
      span.setStatus({
        code: SpanStatusCode.ERROR,
        message: error instanceof Error ? error.message : String(error),
      });
      throw error;
    } finally {
      span.end();
    }
  });
}
