// R10 (R-elicit): Wakeshift's form in Codex's own dialog. The Codex
// driver's reading of the form (screens captured from Codex, with the
// message as the core writes it now), the CI app's
// stateful mode (WB_CI_STATEFUL=1) with a form sent and answered during a
// call, and R10's grading helpers.

import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { Client, StreamableHTTPClientTransport } from "@modelcontextprotocol/client";
import { Wakeshift } from "@wakeshift/sdk";
import { addedPart, ciApp, clientOf, formAnswersIn, formsIn, rewritingArm, statefulSessions } from "../app/ci.js";
import { Codex, elicitForm, isApproval } from "../drivers/codex/index.js";
import { tail } from "../drivers/base.js";
import type { AnswerPolicy, PromptRecord } from "../drivers/types.js";
import type { Lane } from "../src/lane.js";
import { grade, parseTiers } from "../src/grade.js";
import { parseScenarios, REALISTIC, REALISTIC_LANES, SCENARIO_TITLES, type ScenarioResult } from "../src/model.js";
import { formAnswer, noFormReason, r10Outcome } from "../src/realistic.js";

// ── The driver's reading of the form ────────────────────────────────────

const HEAD = `
  >_ OpenAI Codex (v0.170.0)
     /private/tmp/wbe/e2e-62-3/home/work


› Kick off a build of main and let me know when it's done.


• I'll start a build of main and watch it until it finishes.

• Calling shipyard.watch_build
    + Show details



`;

// The message as the core writes it, in Codex's bottom pane as a captured screen shows a form.
const FORM = `${HEAD}  Field 1/1 (1 required unanswered)
  Wakeshift, for Shipyard:
  Shipyard sends its notifications through Wakeshift. To be told here when they arrive, even while you're away, install Wakeshift's plugin for Codex once. Wakeshift makes this plugin, not Shipyard.
  1. In a terminal (with the same CODEX_HOME as this Codex, if you set one), run:
     codex plugin marketplace add wakeshift/agent-plugins@main
     codex plugin add wakeshift@wakeshift
  2. Restart Codex and reopen this conversation: in the CLI, /quit, then codex resume 019a-thread; in the desktop app or your IDE, restart Codex and reopen it.
  3. When Codex asks about hooks, choose "Review hooks" and trust Wakeshift's three hooks (or "Trust all and continue" if only Wakeshift's are listed).
  Already installed it? Skip step 1: just restart Codex and trust Wakeshift's hooks.

  Notifications in this conversation
  › 1. Not now
    2. I'll set it up
  enter to submit | esc to cancel
`;

// The same at 90 columns, wrapped as Codex wraps it.
const FORM_90 = `${HEAD}  Field 1/1 (1 required unanswered)
  Wakeshift, for Shipyard:
  Shipyard sends its notifications through Wakeshift. To be told here when they
  arrive, even while you're away, install Wakeshift's plugin for Codex once.
  1. In a terminal (with the same CODEX_HOME as this Codex, if you set one), run:
     codex plugin marketplace add wakeshift/agent-plugins@main
     codex plugin add wakeshift@wakeshift
  2. Restart Codex and reopen this conversation: in the CLI, /quit, then codex
  resume 019a-thread; in the desktop app or your IDE, restart Codex and reopen it.
  3. When Codex asks about hooks, choose "Review hooks" and trust Wakeshift's
  three hooks (or "Trust all and continue" if only Wakeshift's are listed).

  Notifications in this conversation
  › 1. Not now
    2. I'll set it up
  enter to submit | esc to cancel
`;

// The MCP tool approval, a form of Codex's own (captured).
const APPROVAL = `${HEAD}  Field 1/1
  Allow the shipyard MCP server to run tool "watch_build"?
  number: 413
  › 1. Allow                   Run the tool and continue
    2. Allow for this session  Run the tool and remember this choice for this session
    3. Always allow            Run the tool and remember this choice for future tool calls
    4. Cancel                  Cancel this tool call
  enter to submit | esc to cancel
`;

// An earlier form, before the SDK wrote its first line (and with the options the other way round).
const PROTOTYPE = `${HEAD}  Field 1/1 (1 required unanswered)
  Wakebench sends its notifications through Wakeshift. Set it up once, and this conversation is told when bench events arrive, even while you're away.

  1. In a terminal, run:
     codex plugin marketplace add wakeshift/agent-plugins@main && codex plugin add wakeshift@wakeshift
  2. Quit Codex (/quit) and reopen this conversation with: codex resume --last
  3. When Codex asks, trust the Wakeshift hooks ("Trust all and continue").

  Notifications in this conversation
  › 1. I'll set it up
    2. Not now
  enter to submit | esc to cancel
`;

test("elicitForm: Wakeshift's form in Codex's bottom pane, wrapped or not", () => {
  for (const screen of [FORM, FORM_90]) {
    const f = elicitForm(screen);
    assert.ok(f, "recognized");
    assert.equal(f.app, "Shipyard");
    assert.deepEqual(
      f.options.map((o) => [o.n, o.label, o.cursor]),
      [
        [1, "Not now", true],
        [2, "I'll set it up", false],
      ],
      "the field's options, not the steps' numbers",
    );
  }
});

test("elicitForm: not Codex's approval, not the prototype's form without Wakeshift's line, not a transcript quoting it", () => {
  assert.equal(elicitForm(APPROVAL), null);
  assert.equal(elicitForm(PROTOTYPE), null, "no \"Wakeshift, for\" line");
  // The agent quoting the form's first line above an approval: the approval's the dialog.
  const quoted = APPROVAL.replace("• Calling shipyard.watch_build", "• Wakeshift, for Shipyard:\n  Notifications in this conversation\n  1. Not now\n  2. I'll set it up\n\n• Calling shipyard.watch_build");
  assert.equal(elicitForm(quoted), null);
  // The form answered: the composer is back, no submit line.
  assert.equal(elicitForm(FORM.replace("  enter to submit | esc to cancel\n", "\n› Ask Codex to do anything\n")), null);
  // Another field: not Wakeshift's.
  assert.equal(elicitForm(FORM.replace("Notifications in this conversation", "Pick a color")), null);
});

test("the approval branch doesn't take the form for an approval (or its install commands for the installer)", () => {
  assert.equal(isApproval(tail(APPROVAL, 14)), true);
  for (const screen of [FORM, FORM_90]) assert.equal(isApproval(tail(screen, 14)), false);
});

/** A lane that records keys and prompts, for driving answer() without tmux. */
function fakeLane(): { lane: Lane; keys: string[]; prompts: Omit<PromptRecord, "at">[]; state: Record<string, unknown> } {
  const keys: string[] = [];
  const prompts: Omit<PromptRecord, "at">[] = [];
  const state: Record<string, unknown> = {};
  const lane = {
    session: "wb-test",
    state,
    tmux: { keys: async (_s: string, k: string) => void keys.push(k) },
    prompted: (r: Omit<PromptRecord, "at">) => (prompts.push(r), { ...r, at: "" }),
    screen: async () => FORM,
  } as unknown as Lane;
  return { lane, keys, prompts, state };
}

test("answer: the form gets \"I'll set it up\" under R10's policy, \"Not now\" otherwise, recorded as a form", async () => {
  const codex = new Codex();
  const setup = fakeLane();
  const what = await codex.answer(setup.lane, FORM, { allowInstall: false, form: "setup" } as AnswerPolicy);
  assert.equal(what, "answered Wakeshift's form: I'll set it up");
  assert.deepEqual(setup.keys, ["Down", "Enter"], "from the cursor on 1 down to 2, then submit");
  assert.equal(setup.prompts.length, 1);
  assert.equal(setup.prompts[0].kind, "form");
  assert.equal(setup.prompts[0].chosen, "I'll set it up");
  assert.deepEqual(setup.prompts[0].options, ["Not now", "I'll set it up"]);

  const later = fakeLane();
  assert.equal(await codex.answer(later.lane, FORM_90, { allowInstall: false }), "answered Wakeshift's form: Not now");
  assert.deepEqual(later.keys, ["Enter"], "the cursor's already on Not now");
  assert.equal(later.prompts[0].chosen, "Not now");
  // Never Escape (the installer's denial): the form's commands aren't an approval.
  assert.ok(!later.keys.includes("Escape"));
});

// ── The CI app's stateful mode ──────────────────────────────────────────

const vectors = JSON.parse(readFileSync(new URL("../../../sdk/typescript/test/client-requests.json", import.meta.url), "utf8"));
const KEY = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV";

/** A core that answers what the CI app's calls need, and adds the form to a call that offers it. */
function fakeCore() {
  const bodies: { path: string; body: any }[] = [];
  const fetchFn = (async (url: string | URL | Request, init?: RequestInit) => {
    const path = String(url).replace("https://core.test/api/apps/v1", "");
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    bodies.push({ path, body });
    const json = path.startsWith("/manifest")
      ? {}
      : path === "/event-subscriptions"
        ? { event_subscription: { id: "sub-1", created: true }, session: "wsc_s1", pending: "pa_1" }
        : path === "/calls/addition"
          ? { text: "Today's text.", trailer: "", ...(body?.call?.client_requests?.includes("elicit.form") ? { requests: [vectors.validate[0].request] } : {}) }
          : path === "/app"
            ? { id: "shipyard", name: "Shipyard" }
            : path === "/requests/outcome"
              ? { recorded: true }
              : {};
    return new Response(JSON.stringify(json), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  const wake = new Wakeshift({ key: KEY, core: "https://core.test", fetch: fetchFn, logLevel: "off" });
  return { wake, bodies };
}

async function withCi<T>(env: Record<string, string>, f: (base: string, core: ReturnType<typeof fakeCore>) => Promise<T>): Promise<T> {
  const was = Object.fromEntries(Object.keys(env).map((k) => [k, process.env[k]]));
  Object.assign(process.env, env);
  const core = fakeCore();
  try {
    const server = ciApp({ wake: core.wake, run: "r", controlToken: "tok", wait: false });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
    const a = server.address();
    try {
      return await f(`http://127.0.0.1:${typeof a === "object" && a ? a.port : 0}`, core);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((r) => server.close(() => r()));
      await core.wake.close();
    }
  } finally {
    for (const [k, v] of Object.entries(was)) if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
}

async function codexClient(base: string, lane: string, answer: ((p: any) => any) | null): Promise<{ c: Client; asked: any[] }> {
  const c = new Client({ name: "codex-mcp-client", version: "0.170.0" }, answer ? ({ capabilities: { elicitation: { form: {} } } } as any) : undefined);
  const asked: any[] = [];
  if (answer)
    (c as any).setRequestHandler("elicitation/create", async (req: any) => {
      asked.push(req.params);
      return answer(req.params);
    });
  await c.connect(new StreamableHTTPClientTransport(new URL(`${base}/mcp/${lane}`)));
  return { c, asked };
}

const laneState = async (base: string, lane: string) => (await (await fetch(`${base}/control/lane?lane=${lane}`, { headers: { authorization: "Bearer tok" } })).json()) as any;

test("WB_CI_STATEFUL=1: the form goes out during watch_build, the answer comes back to the session, and both are recorded", async () => {
  await withCi({ WB_CI_STATEFUL: "1" }, async (base, core) => {
    const { c, asked } = await codexClient(base, "codex-re", () => ({ action: "accept", content: { choice: "setup" } }));
    try {
      const r = (await c.callTool({ name: "watch_build", arguments: { number: 412 } })) as any;
      assert.ok(!r.isError);
      assert.equal(asked.length, 1, "the form was shown once");
      assert.ok(asked[0].message.startsWith("Wakeshift, for Shipyard:\n"), asked[0].message);
      const texts = (r.content as any[]).filter((x) => x.type === "text").map((x) => x.text);
      assert.equal(texts[0], "Set up.", "the outcome's text, before the app's (placement before)");
      const add = core.bodies.find((b) => b.path === "/calls/addition")!.body;
      assert.deepEqual(add.call.client_requests, ["elicit.form"]);
      assert.deepEqual(core.bodies.filter((b) => b.path === "/requests/outcome").map((b) => b.body.outcome), ["choice=setup"]);
      const st = await laneState(base, "codex-re");
      assert.equal(st.stateful, true);
      assert.equal(st.clients.length, 1);
      assert.equal(st.clients[0].client, "codex-mcp-client");
      assert.equal(st.clients[0].elicitation_form, true);
      assert.equal(st.forms.length, 1);
      assert.equal(st.forms[0].first, "Wakeshift, for Shipyard:");
      assert.equal(st.forms[0].tool, "watch_build");
      assert.equal(st.forms[0].field, "Notifications in this conversation");
      assert.deepEqual(st.forms[0].options.map((o: any) => o.title), ["Not now", "I'll set it up"]);
      assert.equal(st.forms[0].answer.action, "accept");
      assert.equal(st.forms[0].answer.choice, "setup");
      // The call's record: Wakeshift's part is the outcome's text, not the app's own block (which came after it).
      const call = st.calls.find((x: any) => x.tool === "watch_build");
      assert.ok(call, "the call is recorded");
      assert.equal(call.subscription, "sub-1");
      assert.match(call.added, /^Set up\./);
      assert.doesNotMatch(call.added, /Watching build #412/);
      assert.match(call.text, /Watching build #412/);
      // Another lane's URL with this session's ID is refused.
      const other = await fetch(`${base}/mcp/codex-r`, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json, text/event-stream", "mcp-session-id": (c as any).transport?.sessionId ?? "x", "mcp-protocol-version": "2025-06-18" },
        body: JSON.stringify({ jsonrpc: "2.0", id: 9, method: "tools/list" }),
      });
      assert.equal(other.status, 404);
      await other.text();
    } finally {
      await c.close();
    }
  });
});

test("WB_CI_STATEFUL=1: a client that doesn't take forms gets none, and \"Not now\" is recorded as later", async () => {
  await withCi({ WB_CI_STATEFUL: "1" }, async (base, core) => {
    const none = await codexClient(base, "codex-a", null);
    try {
      await none.c.callTool({ name: "watch_build", arguments: { number: 412 } });
      assert.equal(core.bodies.find((b) => b.path === "/calls/addition")!.body.call.client_requests, undefined);
      const st = await laneState(base, "codex-a");
      assert.equal(st.clients[0].elicitation_form, false);
      assert.equal(st.forms.length, 0);
    } finally {
      await none.c.close();
    }
    const later = await codexClient(base, "codex-b", () => ({ action: "accept", content: { choice: "later" } }));
    try {
      await later.c.callTool({ name: "watch_build", arguments: { number: 412 } });
      const st = await laneState(base, "codex-b");
      assert.equal(st.forms[0].answer.choice, "later");
      assert.equal(formAnswer(st.forms, []), "later");
    } finally {
      await later.c.close();
    }
  });
});

test("without WB_CI_STATEFUL the CI app serves statelessly, so no call offers a form", async () => {
  await withCi({}, async (base, core) => {
    const { c, asked } = await codexClient(base, "codex-r", () => ({ action: "accept", content: { choice: "setup" } }));
    try {
      const r = (await c.callTool({ name: "watch_build", arguments: { number: 412 } })) as any;
      assert.ok(!r.isError);
      assert.equal(asked.length, 0);
      assert.equal(core.bodies.find((b) => b.path === "/calls/addition")!.body.call.client_requests, undefined);
      const st = await laneState(base, "codex-r");
      assert.equal(st.stateful, false);
      assert.equal(st.calls.find((x: any) => x.tool === "watch_build").added, "Today's text.");
    } finally {
      await c.close();
    }
  });
});

test("stateful mode refuses the experiments that rewrite a result whole", () => {
  assert.equal(statefulSessions({ WB_CI_STATEFUL: "1" }), true);
  assert.equal(statefulSessions({}), false);
  assert.equal(rewritingArm({}), null);
  assert.equal(rewritingArm({ WB_CI_WATCH_DESCRIPTION: "explicit" }), null, "a description isn't a rewrite");
  assert.equal(rewritingArm({ WB_CI_WATCH_DESCRIPTION: "status" }), "WB_CI_WATCH_DESCRIPTION=status");
  assert.equal(rewritingArm({ WB_CI_PLUGIN_ASK: "v1" }), "WB_CI_PLUGIN_ASK=v1");
  const was = { s: process.env.WB_CI_STATEFUL, d: process.env.WB_CI_WATCH_DESCRIPTION };
  process.env.WB_CI_STATEFUL = "1";
  process.env.WB_CI_WATCH_DESCRIPTION = "ask3";
  try {
    assert.throws(() => ciApp({ wake: fakeCore().wake, run: "r", controlToken: "tok" }), /doesn't run with WB_CI_WATCH_DESCRIPTION=ask3/);
  } finally {
    if (was.s === undefined) delete process.env.WB_CI_STATEFUL;
    else process.env.WB_CI_STATEFUL = was.s;
    if (was.d === undefined) delete process.env.WB_CI_WATCH_DESCRIPTION;
    else process.env.WB_CI_WATCH_DESCRIPTION = was.d;
  }
});

test("the record's readers: forms in a response, answers in a request, the client's declaration, Wakeshift's part", () => {
  const forms = formsIn([
    { jsonrpc: "2.0", id: 0, method: "elicitation/create", params: { message: "Wakeshift, for Shipyard:\nShipyard sends…", requestedSchema: vectors.validate[0].request.schema } },
    { jsonrpc: "2.0", id: 0, result: { content: [] } },
  ]);
  assert.deepEqual(forms, [{ id: 0, first: "Wakeshift, for Shipyard:", field: "Notifications in this conversation", options: [{ value: "later", title: "Not now" }, { value: "setup", title: "I'll set it up" }] }]);
  assert.deepEqual(formAnswersIn({ jsonrpc: "2.0", id: 0, result: { action: "accept", content: { choice: "setup" } } }), [{ id: 0, action: "accept", choice: "setup" }]);
  assert.deepEqual(formAnswersIn([{ jsonrpc: "2.0", id: 1, result: { action: "decline" } }, { jsonrpc: "2.0", id: 2, method: "tools/list" }]), [{ id: 1, action: "decline", choice: null }]);
  assert.deepEqual(formAnswersIn({ jsonrpc: "2.0", id: 3, result: { content: [] } }), [], "a response that isn't a form's answer");
  assert.equal(clientOf({ params: { protocolVersion: "2025-06-18", clientInfo: { name: "codex-mcp-client", version: "0.170.0" }, capabilities: { elicitation: {} } } }).elicitation_form, true, "2025-06-18's {} is forms");
  assert.equal(clientOf({ params: { capabilities: { elicitation: { url: {} } } } }).elicitation_form, false);
  assert.equal(clientOf({ params: { capabilities: {} } }).elicitation_form, false);
  const t = (text: string) => ({ type: "text" as const, text });
  assert.deepEqual(addedPart([t("Set up."), t("Watching."), t("trailer")], { own: 1, ownText: "Watching." }), [t("Set up."), t("trailer")]);
  assert.deepEqual(addedPart([t("Watching."), t("Today's text.")], { own: 1 }), [t("Today's text.")]);
  assert.deepEqual(addedPart([t("all")], undefined), [t("all")]);
});

// ── R10's grading ───────────────────────────────────────────────────────

test("R10 is a realistic scenario of its own, by name and in ranges, not in the repeated realistic lanes", () => {
  assert.deepEqual(parseScenarios("R10"), ["R10"]);
  assert.deepEqual(parseScenarios("R9-R10"), ["R9", "R10"]);
  assert.ok(REALISTIC.includes("R10"));
  assert.ok(!REALISTIC_LANES.includes("R10"));
  assert.match(SCENARIO_TITLES.R10, /Codex's own dialog/);
  assert.deepEqual(parseScenarios("R1-R6"), ["R1", "R2", "R3", "R4", "R5", "R6"]);
});

test("formAnswer: the app's record of the answer first, else the driver's", () => {
  assert.equal(formAnswer([{ answer: { action: "accept", choice: "setup", at: "" } }], []), "setup");
  assert.equal(formAnswer([{ answer: { action: "decline", choice: null, at: "" } }], [{ kind: "form", chosen: "I'll set it up" }]), "decline");
  assert.equal(formAnswer([{ answer: null }], [{ kind: "approval", chosen: "Allow" }, { kind: "form", chosen: "I'll set it up" }]), "setup");
  assert.equal(formAnswer([], [{ kind: "form", chosen: "Not now" }]), "later");
  assert.equal(formAnswer([], [{ kind: "approval", chosen: "Allow" }]), null);
});

test("noFormReason: why no form showed", () => {
  assert.match(noFormReason({ stateful: false, clients: [], added: "", offered: false }), /WB_CI_STATEFUL=1/);
  assert.match(noFormReason({ stateful: true, clients: [{ client: "codex-mcp-client", elicitation_form: false }], added: "x", offered: true }), /didn't declare elicitation forms/);
  assert.match(noFormReason({ stateful: true, clients: [{ client: "codex-mcp-client", elicitation_form: true }], added: "Install Wakeshift's plugin…", offered: true }), /text offer instead/);
  assert.match(noFormReason({ stateful: true, clients: [], added: "", offered: false }), /no Wakeshift text/);
});

const ok = { subscribed: true, formShown: true, noForm: "no form: x", formAnswer: "setup", installed: true, connected: true, woke: true };

test("r10Outcome: works only when shown, answered, installed, connected and woken; no form is n/a, never a pass", () => {
  assert.deepEqual(r10Outcome(ok), { status: "ran", works: true });
  assert.deepEqual(r10Outcome({ ...ok, formShown: false }), { status: "n/a", works: false, reason: "no form: x" });
  assert.equal(r10Outcome({ ...ok, subscribed: false }).works, false);
  assert.equal(r10Outcome({ ...ok, subscribed: false }).status, "ran");
  assert.match(r10Outcome({ ...ok, formAnswer: "later" }).reason ?? "", /answered later/);
  assert.match(r10Outcome({ ...ok, formAnswer: null }).reason ?? "", /never answered/);
  assert.match(r10Outcome({ ...ok, installed: false }).reason ?? "", /nothing was installed/);
  assert.match(r10Outcome({ ...ok, connected: false }).reason ?? "", /never connected/);
  assert.match(r10Outcome({ ...ok, woke: false }).reason ?? "", /didn't wake/);
});

test("R10's bars (tiers.yaml): the agent didn't run the install, connected and woke soon; n/a grades n/a", () => {
  const tiers = parseTiers(readFileSync(new URL("../../tiers.yaml", import.meta.url), "utf8"));
  const t1 = tiers.tiers.find((t) => t.id === "T1")!;
  const cell = (metrics: Record<string, any>, extra: Partial<ScenarioResult> = {}): ScenarioResult => grade({ id: "R10", harness: "codex", status: "ran", works: true, metrics, steps: [], notes: [], ...extra }, t1);
  const good = { form_shown: true, form_answer: "setup", agent_ran_install: false, installed: true, trusted: true, connected: true, connected_ms: 9000, woke: true, wake_ms: 7000, polled_instead: false };
  assert.equal(cell(good).grade, "pass");
  assert.equal(cell({ ...good, agent_ran_install: true }).grade, "below-bar");
  assert.equal(cell({ ...good, wake_ms: 40_000 }).grade, "below-bar");
  assert.equal(cell({ ...good, connected_ms: 90_000 }).grade, "below-bar");
  assert.equal(cell({}, { status: "n/a", works: false, reason: "no form: x" }).grade, "n/a");
  assert.equal(cell(good).key, "form: setup, conn 9.0s, woke in 7.0s");
});
