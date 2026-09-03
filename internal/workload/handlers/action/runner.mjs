import { readFileSync } from "node:fs";
import vm from "node:vm";

const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const request = JSON.parse(Buffer.concat(chunks).toString("utf8"));
const source = readFileSync(process.argv[2], "utf8");
const state = new Map();
const artifacts = [];
const safeConsole = Object.freeze({
  debug: (...values) => console.error("[action:debug]", ...values),
  info: (...values) => console.error("[action:info]", ...values),
  warn: (...values) => console.error("[action:warn]", ...values),
  error: (...values) => console.error("[action:error]", ...values),
  log: (...values) => console.error("[action:log]", ...values),
});
const actionContext = Object.freeze({
  config: Object.freeze(request.config ?? {}),
  state: Object.freeze({
    get: (key) => state.get(String(key)),
    set: (key, value) => { state.set(String(key), value); },
    patch: (values) => {
      for (const [key, value] of Object.entries(values ?? {})) state.set(key, value);
    },
  }),
  artifacts: Object.freeze({
    publish: (artifact) => {
      const safe = structuredClone(artifact);
      artifacts.push(safe);
      return safe;
    },
  }),
  logger: safeConsole,
});
const sandbox = {
  console: safeConsole,
  TextEncoder,
  TextDecoder,
  URL,
  URLSearchParams,
  structuredClone,
  setTimeout,
  clearTimeout,
};
const context = vm.createContext(sandbox, {
  name: "beam-studio-action",
  codeGeneration: { strings: false, wasm: false },
});

try {
  const actionModule = new vm.SourceTextModule(source, {
    context,
    initializeImportMeta: (meta) => Object.freeze(meta),
    importModuleDynamically: () => {
      throw new Error("Studio sandbox does not allow dynamic imports");
    },
  });
  await actionModule.link(() => {
    throw new Error("Studio sandbox requires a bundled action and does not allow imports");
  });
  await actionModule.evaluate({ timeout: request.cpuTimeoutMs });
  const execute = actionModule.namespace.execute ?? actionModule.namespace.default?.execute;
  if (typeof execute !== "function") throw new Error("Action does not export execute(input, context)");
  context.__beamExecute = execute;
  context.__beamInput = structuredClone(request.input ?? {});
  context.__beamContext = actionContext;
  vm.runInContext("__beamResult = Promise.resolve(__beamExecute(__beamInput, __beamContext))", context, {
    timeout: request.cpuTimeoutMs,
  });
  const result = await context.__beamResult;
  process.stdout.write(JSON.stringify({ ok: true, result: structuredClone(result), artifacts }));
} catch (error) {
  process.stdout.write(JSON.stringify({
    ok: false,
    error: { name: error?.name ?? "ActionError", message: String(error?.message ?? error) },
  }));
  process.exitCode = 1;
}
