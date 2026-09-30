"""Vitest 3/4 programmatic selection before executing test files."""
from __future__ import annotations

import json


BOOTSTRAP = r'''
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const request = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const { createVitest, parseCLI } = await import(pathToFileURL(request.node_api).href);
process.env.TEST = 'true';
process.env.VITEST = 'true';
process.env.NODE_ENV ??= 'test';
const { filter, options } = parseCLI(['vitest', ...request.args]);
options.watch = false;
const root = fs.realpathSync(request.root);
const inside = (file, directory) => {
  const relative = path.relative(directory, file);
  return relative === '' || (!relative.startsWith('..' + path.sep) && relative !== '..' && !path.isAbsolute(relative));
};
const selected = request.selected.map(([file, directory]) => [fs.realpathSync(file), directory]);
const allowed = file => inside(file, root) && !inside(file, path.join(root, '.agentgo'))
  && selected.some(([target, directory]) => file === target || directory && inside(file, target));
const ctx = await createVitest('test', options);
try {
  // Glob retains every upstream project, include/exclude rule, pool and environment.
  // Select specifications by exact path BEFORE running any test module.
  const discovered = await ctx.globTestSpecifications(filter);
  const scoped = discovered.filter(spec => allowed(fs.realpathSync(spec.moduleId)));
  if (!scoped.length) throw new Error('No Vitest specifications in the declared source scope');
  fs.writeFileSync(request.inventory, JSON.stringify({
    schema: 'agentgo.vitest-selection/v1',
    selected: scoped.map(spec => ({ file: spec.moduleId, project: spec.project.name })),
    excluded: discovered.filter(spec => !scoped.includes(spec)).map(spec => ({ file: spec.moduleId, project: spec.project.name })),
  }, null, 2) + '\n');
  // Standalone initialization otherwise launches browsers from unrelated projects.
  ctx.projects = ctx.projects.filter(project => scoped.some(spec => spec.project === project));
  await ctx.init();
  await ctx.runTestSpecifications(scoped);
} finally {
  await ctx.close();
}
'''


def command_files(root, output, selected, arguments):
    helper = output / 'vitest-scope.mjs'
    request = output / 'vitest-scope.json'
    helper.write_text(BOOTSTRAP, encoding='utf-8', newline='\n')
    request.write_text(json.dumps({'root': str(root),
        'node_api': str((root / 'node_modules/vitest/dist/node.js').resolve(strict=True)),
        'args': arguments, 'selected': [(str(path), directory) for path, directory in selected],
        'inventory': str(output / 'vitest-selection.json')}, indent=2) + '\n', encoding='utf-8')
    return helper, request
