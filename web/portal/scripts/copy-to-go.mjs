import { copyFileSync, mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const source = resolve(here, '../dist/index.html');
const target = resolve(here, '../../../cmd/api-server/portal.html');
mkdirSync(dirname(target), { recursive: true });
copyFileSync(source, target);
console.log(`portal.html -> ${target}`);
