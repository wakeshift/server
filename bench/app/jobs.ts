// S16 and S17's job tools on the toy app (server.ts registers them on the
// lanes that ask for them), and what they say, which the bench reads too
// (src/scenarios.ts tells the tools' output from the agent's own words).

/** The extra tools a lane can have (S16, S17): never on a lane that didn't ask for them. */
export const JOB_TOOLS = ["job_status", "start_job"] as const;
export type JobTool = (typeof JOB_TOOLS)[number];

/** A lane's options (POST /control/options), set before its harness connects. */
export interface LaneOptions {
  /** S16: every request on the lane is disguised as an agent Wakeshift doesn't recognize (server.ts disguise). */
  unrecognized?: boolean;
  /** S16, S17: the extra tools the lane's MCP server lists. */
  tools?: JobTool[];
}

export const START_JOB_DESCRIPTION = "Start a bench job. Each call starts a new job. You'll be notified in this conversation when it finishes.";
export const JOB_STATUS_DESCRIPTION = "Get the status of the bench job: whether it's still running, or its result.";

/** What job_status says: the lane's latest bench event is the job's result, as a vendor's status tool reads its own records. */
export function jobStatusText(latest: { code: string; tag?: string; at: string } | undefined, started: number): string {
  if (!latest) return started > 1 ? `${started} bench jobs started. Status: running. No result yet.` : "Status: running. No result yet.";
  return `Status: finished. Result: ${latest.tag ?? "done"} (${latest.code}, at ${latest.at}).`;
}

/** What start_job says. */
export const startedText = (n: number) => `Started bench job #${n}. It takes a few minutes.`;

/** The bench event's text (server.ts benchEvent), for telling it from the agent's words. */
export const benchEventText = (code: string, tag?: string) => `Bench event ${code}${tag ? ` (${tag})` : ""}.`;

/** The name, User-Agent and title an unrecognized lane's requests carry: no harness Wakeshift knows. */
export const UNKNOWN_CLIENT = { name: "acme-agent", title: "Acme Agent", userAgent: "acme-agent/1.0" };
