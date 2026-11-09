import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const source = resolve(here, '../dist/index.html');
const target = resolve(here, '../../../cmd/api-server/portal.html');
mkdirSync(dirname(target), { recursive: true });
writeFileSync(target, readFileSync(source, 'utf8').replace(/\r+\n/g, '\n'));
console.log(`portal.html -> ${target}`);
