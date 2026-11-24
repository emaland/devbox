// A stand-in for the AHP gateway in the lifecycle VM tests (design note
// 031, section 6). It speaks only what the lifecycle services use: GET
// /status with the token, /healthz, and a clean stop on SIGTERM. Its state
// is files in the state dir, so a test can make it busy and see what
// happened across a reboot.
//
//   <stateDir>/turns.json    {"active": n, "waitingForUser": m}, set by the test
//   <stateDir>/clean-stops   one line per clean stop
//   <stateDir>/interrupted   chats whose latest turn a clean stop interrupted
//                            (C2's resume.interrupted). The stub has one chat
//                            and no Try Again: a stop with any active turn,
//                            waiting for a person or not, makes it 1, and a
//                            stop with none leaves it as it was
//   <stateDir>/continued     one line per turn continued at start; since D24
//                            the gateway never does that, so this stays empty
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

// D24: interrupted turns are restored and wait for a person. None is
// continued, whatever the config says.
const interrupted = Number(read("interrupted", "0"));
if (config.resume && config.resume.onStart === "continue") {
  fs.appendFileSync(file("continued"), "config asked for continuation\n");
}

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
    resume: { policy: "off", source: "default", maxAttemptsPerTurn: 0, pending: 0, resumedSinceStart: 0, failed: 0, skipped: 0, interrupted },
  }));
});
server.listen(config.listener.port, "127.0.0.1");

process.on("SIGTERM", () => {
  fs.appendFileSync(file("clean-stops"), `${new Date().toISOString()}\n`);
  // C3 ends every active turn host_restarted, one waiting for a person too.
  if (turns().active > 0) fs.writeFileSync(file("interrupted"), "1");
  fs.writeFileSync(file("turns.json"), '{"active": 0, "waitingForUser": 0}');
  server.close(() => process.exit(0));
});
