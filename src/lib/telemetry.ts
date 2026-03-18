import { authenticatedHttp } from "./auth.js";
import type { RecallstackConfig } from "./config.js";

type CliTelemetryInput = {
  eventName:
    | "cli.auth.login"
    | "cli.workspace.use"
    | "cli.project.use"
    | "cli.memory.query"
    | "cli.memory.ingest"
    | "cli.memory.source.ingest"
    | "cli.memory.source.query"
    | "cli.memory.source.get"
    | "cli.agent.install"
    | "cli.agent.uninstall"
    | "cli.agent.status";
  operation: string;
  outcome?: "success" | "error" | "cancelled";
  projectId?: string;
  durationMs?: number;
  attributes?: Record<string, string | number | boolean | null>;
};

export async function emitCliTelemetry(config: RecallstackConfig, input: CliTelemetryInput): Promise<void> {
  try {
    await authenticatedHttp("/v1/telemetry/events", {
      method: "POST",
      body: {
        source: "cli",
        event_name: input.eventName,
        operation: input.operation,
        outcome: input.outcome || "success",
        project_id: input.projectId,
        duration_ms: input.durationMs,
        attributes: input.attributes,
      },
    }, config);
  } catch {
    // best effort
  }
}
