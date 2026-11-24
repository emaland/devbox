// A stand-in for the AHP gateway in the lifecycle VM tests (design note
// 031, section 6). It speaks only what the lifecycle services use: GET
// /status with the token, /healthz, and a clean stop on SIGTERM. Its state
// is files in the state dir, so a test can make it busy and see what
// happened across a reboot.
//
//   <stateDir>/turns.json    {"active": n, "waitingForUser": m}, set by the test
//   <stateDir>/clean-stops   one line per clean stop
//   <stateDir>/interrupted   turns active at the last clean stop
//   <stateDir>/continued     one line per continuation, written at start
"use strict";
const fs = require("fs");
const http = require("http");
const path = require("path");

const configPath = process.argv[process.argv.indexOf("--config") + 1];
const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
const stateDir = process.env.AHP_GATEWAY_STATE_DIR;
const file = (name) => path.join(stateDir, name);
const read = (name, fallback) => {
  try { return fs.readFileSync(file(name), "utf8"); } catch { return fallback; }
};
const turns = () => JSON.parse(read("turns.json", '{"active": 0, "waitingForUser": 0}'));

const policy = (config.resume && config.resume.onStart) || "off";
let resumedSinceStart = 0;
const interrupted = Number(read("interrupted", "0"));
if (interrupted > 0 && policy === "continue") {
  for (let i = 0; i < interrupted; i++) fs.appendFileSync(file("continued"), "turn\n");
  resumedSinceStart = interrupted;
  fs.writeFileSync(file("turns.json"), JSON.stringify({ active: interrupted, waitingForUser: 0 }));
}
fs.writeFileSync(file("interrupted"), "0");

const server = http.createServer((request, response) => {
  if (request.url === "/healthz") {
    response.end("ok");
    return;
  }
  if (request.url === "/ahp") {
    response.statusCode = 401;
    response.end();
    return;
  }
  if (request.url !== "/status") {
    response.statusCode = 404;
    response.end();
    return;
  }
  if (request.headers.authorization !== `Bearer ${config.auth.token}`) {
    response.statusCode = 401;
    response.end();
    return;
  }
  response.setHeader("Content-Type", "application/json");
  response.end(JSON.stringify({
    ok: true, version: "0.0.0-stub", sessions: { total: 1 }, clients: 0,
    turns: turns(),
    resume: { policy, source: "config", maxAttemptsPerTurn: 1, pending: 0, resumedSinceStart, failed: 0, skipped: 0 },
  }));
});
server.listen(config.listener.port, "127.0.0.1");

process.on("SIGTERM", () => {
  fs.appendFileSync(file("clean-stops"), `${new Date().toISOString()}\n`);
  fs.writeFileSync(file("interrupted"), String(turns().active - turns().waitingForUser));
  fs.writeFileSync(file("turns.json"), '{"active": 0, "waitingForUser": 0}');
  server.close(() => process.exit(0));
});
