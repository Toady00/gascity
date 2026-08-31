// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// OpenCode's plugin API is ESM and hook-oriented:
//   - event() is side-effect-only (no prompt injection)
//   - experimental.chat.system.transform mutates output.system
//   - experimental.session.compacting → inject context before compaction
//
// Gas City uses:
//   - session.created / session.compacted → gc prime --hook (side effects such
//     as session-id persistence and poller bootstrap)
//   - experimental.session.compacting → gc handoff --auto "context cycle"
//     and inject the handoff confirmation into the compaction context
//   - experimental.chat.system.transform → inject gc prime --hook, queued
//     nudges, and unread mail into the system prompt for each turn
//
// Injection deliberately does NOT use chat.message. OpenCode awaits that hook
// before it persists the user's message (SessionPrompt calls updateMessage /
// updatePart only after the trigger returns), so building the prefix there
// delays the send acknowledgement itself rather than just the reply.
//
// OpenCode auto-discovers this file for EVERY OpenCode launch in the work
// dir, including a developer's personal session started by hand. The plugin
// therefore registers hooks only when gc's session identity is present in
// the environment; otherwise every lifecycle call it would make is
// guaranteed to fail (gc's session-facing commands require that identity)
// and the failures surface as warnings in a session Gas City does not own.

import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GC_OPENCODE_HOOK_VERSION = 7;
const GC_BIN = process.env.GC_BIN || "gc";
// Every gc call this plugin makes shares one timeout. Optional per-turn
// injection used to carry a shorter budget of its own, but that was justified
// only while it blocked the send acknowledgement; it no longer does, and the
// stalls it was hedging against were an unclosed child stdin rather than slow
// work.
const COMMAND_TIMEOUT_MS = 30000;
// GC_BIN is the explicit override. The fallback order matches Pi hooks so
// sibling providers resolve the same installed gc before developer-local bins.
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

async function runCommand(directory, args, warnOnFailure, extraEnv = {}) {
  const startedAt = Date.now();
  try {
    // execFile always gives the child a stdin pipe and never closes it, so a
    // gc subcommand that reads hook stdin waits for an EOF that never arrives
    // and is killed when the timeout expires. Close it immediately: these
    // calls send nothing on stdin. (`stdio` is not an execFile option — it is
    // honored by spawn and execFileSync, which is why the pi hook can pass
    // stdio: ["ignore", ...] instead.)
    const pending = execFileAsync(GC_BIN, args, {
      cwd: directory,
      encoding: "utf-8",
      timeout: COMMAND_TIMEOUT_MS,
      env: {
        ...process.env,
        ...extraEnv,
        PATH: PATH_PREFIX + (process.env.PATH || ""),
      },
    });
    pending.child.stdin?.end();
    const { stdout, stderr } = await pending;
    logRunStderr(stderr);
    return stdout.trim();
  } catch (err) {
    if (warnOnFailure) {
      logRunFailure(args, directory, err, Date.now() - startedAt, COMMAND_TIMEOUT_MS);
    }
    return "";
  }
}

async function runWithWarning(directory, ...args) {
  return runCommand(directory, args, true);
}

// Node reports killed=true and signal=SIGTERM both when our own timeout fires
// and when something else terminates the child (a supervisor restart, say), so
// the error alone cannot tell them apart. Report the elapsed time against the
// budget that was in force: a failure at ~3000ms of a 3000ms budget is our
// timeout, one at 200ms is not.
function logRunFailure(args, directory, err, elapsedMs, timeoutMs) {
  try {
    const detail =
      (err && (err.code || err.signal || err.message)) || "unknown error";
    const timing =
      Number.isFinite(elapsedMs) && Number.isFinite(timeoutMs)
        ? ` after ${elapsedMs}ms (budget ${timeoutMs}ms)`
        : "";
    console.warn(
      "gascity opencode plugin:",
      `${GC_BIN} ${args.join(" ")}`,
      "cwd",
      directory,
      `failed${timing}:`,
      detail,
    );
  } catch {
    return;
  }
}

function logRunStderr(stderr) {
  try {
    const detail = String(stderr || "").trim();
    if (detail) {
      console.warn("gascity opencode plugin:", detail);
    }
  } catch {
    return;
  }
}

function unwrapData(result) {
  if (result && typeof result === "object" && "data" in result) {
    return result.data;
  }
  return result;
}

function safeSessionID(sessionID) {
  return String(sessionID || "").replace(/[^A-Za-z0-9_.-]/g, "_");
}

function sessionIDFromEvent(event) {
  return (
    event?.properties?.sessionID ||
    event?.properties?.info?.sessionID ||
    event?.properties?.message?.info?.sessionID ||
    ""
  );
}

// Mirrors gc's own hookHasManagedIdentity (cmd/gc/cmd_prime.go): the session
// lifecycle seeds GC_SESSION_ID, GC_SESSION_NAME and GC_ALIAS into every
// managed session's environment, and GC_AGENT/GC_TEMPLATE come from templated
// starts. A process with none of these was not started by gc, so gc handoff
// would exit 1 ("not in session context") and the injection commands would
// return nothing.
function managedSessionIdentityPresent() {
  return [
    "GC_SESSION_ID",
    "GC_SESSION_NAME",
    "GC_ALIAS",
    "GC_AGENT",
    "GC_TEMPLATE",
  ].some((key) => String(process.env[key] || "").trim() !== "");
}

function providerSessionEnv(sessionID) {
  sessionID = String(sessionID || "");
  const env = { GC_PROVIDER_SESSION_ID_REQUIRED: "opencode" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = sessionID;
  return env;
}

async function mirrorTranscript(directory, client, sessionID) {
  const exportDir = process.env.GC_OPENCODE_TRANSCRIPT_DIR || "";
  const safeID = safeSessionID(sessionID);
  if (!exportDir || !safeID || !client?.session) {
    return;
  }

  try {
    const [infoResult, messagesResult] = await Promise.all([
      client.session.get({ path: { id: sessionID } }),
      client.session.messages({ path: { id: sessionID } }),
    ]);
    const info = unwrapData(infoResult) || {};
    const messages = unwrapData(messagesResult) || [];
    if (!info.directory) {
      info.directory = directory;
    }
    await fs.mkdir(exportDir, { recursive: true });
    const dst = path.join(exportDir, `${safeID}.json`);
    const tmp = `${dst}.tmp`;
    await fs.writeFile(tmp, JSON.stringify({ info, messages }, null, 2));
    await fs.rename(tmp, dst);
  } catch {
    return;
  }
}

export default async function gascityPlugin({ directory, client }) {
  if (!managedSessionIdentityPresent()) {
    // One line so an operator can tell "plugin inactive" from "plugin
    // missing", then stay silent: this OpenCode instance is not gc-managed.
    console.warn(
      "gascity opencode plugin: no Gas City session identity in the environment; hooks disabled for this OpenCode instance",
    );
    return {};
  }
  let cachedPrime = null;
  // experimental.chat.system.transform fires once per model generation, not
  // once per user turn: OpenCode triggers it from Agent.generate, so a turn
  // that dispatches subagents runs the prefix build several times. Draining a
  // consumptive queue there means the queue is emptied repeatedly and the
  // drained items land in whichever generation happened to win the race.
  //
  // Track the turn a user message opened and let the consumptive commands run
  // once per turn. When no turn is known — events not delivered, or a payload
  // without the fields below — this falls back to the previous behavior of
  // running them every time, so nudges are never silently withheld.
  let currentTurnID = "";
  let drainedTurnID = null;

  async function readPrime(force = false, extraEnv = {}) {
    if (force || cachedPrime === null) {
      cachedPrime = await runCommand(directory, ["prime", "--hook"], false, extraEnv);
    }
    return cachedPrime;
  }

  function prependText(existing, prefix) {
    return existing ? prefix + "\n\n" + existing : prefix;
  }

  async function buildPrefix() {
    const prime = await readPrime();
    if (currentTurnID && drainedTurnID === currentTurnID) {
      return prime;
    }
    // Claim the turn before awaiting so concurrent generations cannot both
    // reach the drain.
    drainedTurnID = currentTurnID;
    const [nudges, mail] = await Promise.all([
      runWithWarning(directory, "nudge", "drain", "--inject"),
      runWithWarning(directory, "mail", "check", "--inject"),
    ]);
    return [prime, nudges, mail].filter(Boolean).join("\n\n");
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          {
            const sessionID = sessionIDFromEvent(event);
            await readPrime(true, providerSessionEnv(sessionID));
            await mirrorTranscript(directory, client, sessionID);
          }
          return;
        case "message.updated":
          {
            // A new user message opens a turn.
            const info = event?.properties?.info;
            if (info && info.role === "user" && info.id) {
              currentTurnID = String(info.id);
            }
          }
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        case "session.idle":
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        default:
          return;
      }
    },

    "experimental.chat.system.transform": async (_input, output) => {
      const prefix = await buildPrefix();
      if (prefix) {
        if (output.system[0]) {
          output.system[0] = prependText(output.system[0], prefix);
        } else {
          output.system.unshift(prefix);
        }
      }
    },

    "experimental.session.compacting": async (_input, output) => {
      const handoff = await runWithWarning(directory, "handoff", "--auto", "context cycle");
      if (!handoff) {
        return;
      }
      if (Array.isArray(output?.context)) {
        output.context.push(handoff);
        return;
      }
      try {
        console.warn(
          "gascity opencode plugin: compacting output.context is not an array; skipped handoff injection",
        );
      } catch {
        return;
      }
    },
  };
}
