import { test, expect } from "bun:test";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

const root = import.meta.dir;
const load = (path) => readFileSync(join(root, path), "utf8");
const documents = (path) => load(path).split(/^---\s*$/m).map(Bun.YAML.parse);

test("fixture documents parse and resource identities are unique within each file", () => {
  for (const directory of ["bindings", "git", "bundle"]) {
    for (const file of readdirSync(join(root, directory))) {
      const path = `${directory}/${file}`;
      const items = file.endsWith(".yaml") ? documents(path) : [JSON.parse(load(path))];
      const keys = items.map((item) => `${item.apiVersion}/${item.kind}/${item.metadata.name ?? item.metadata.generateName}`);
      expect(new Set(keys).size).toBe(items.length);
      for (const item of items) expect(item.spec ?? item.data ?? item.stringData).toBeDefined();
    }
  }
});

test("configured binding resources satisfy named references and workspace bindings", () => {
  const items = documents("bindings/resources.yaml");
  const run = JSON.parse(load("bindings/run.json"));
  const pipeline = items.find((item) => item.kind === "Pipeline" && item.metadata.name === run.spec.pipelineRef.name);
  expect(pipeline).toBeDefined();
  for (const binding of run.spec.workspaces) {
    const kind = binding.configMap ? "ConfigMap" : "PersistentVolumeClaim";
    const name = binding.configMap?.name ?? binding.persistentVolumeClaim.claimName;
    expect(items.some((item) => item.kind === kind && item.metadata.name === name)).toBe(true);
  }
  for (const task of pipeline.spec.tasks) {
    const definition = items.find((item) => item.kind === "Task" && item.metadata.name === task.taskRef.name);
    expect(definition).toBeDefined();
    for (const workspace of definition.spec.workspaces) {
      expect(task.workspaces.some((binding) => binding.name === workspace.name)).toBe(true);
    }
    for (const step of definition.spec.steps) {
      for (const entry of step.env ?? []) {
        if (!entry.valueFrom?.secretKeyRef) continue;
        const ref = entry.valueFrom.secretKeyRef;
        const secret = items.find((item) => item.kind === "Secret" && item.metadata.name === ref.name);
        expect(secret?.stringData[ref.key]).toBeDefined();
      }
    }
  }
});

test("bundle contains the Pipeline and Task requested by the two resolution hops", () => {
  const items = documents("bundle/resources.yaml");
  const run = JSON.parse(load("bundle/run.json"));
  const params = Object.fromEntries(run.spec.pipelineRef.params.map(({ name, value }) => [name, value]));
  const pipeline = items.find((item) => item.kind.toLowerCase() === params.kind && item.metadata.name === params.name);
  expect(pipeline).toBeDefined();
  for (const task of pipeline.spec.tasks) {
    const params = Object.fromEntries(task.taskRef.params.map(({ name, value }) => [name, value]));
    expect(task.taskRef.resolver).toBe("bundles");
    expect(items.some((item) => item.kind.toLowerCase() === params.kind && item.metadata.name === params.name)).toBe(true);
  }
});
