import {readdirSync, readFileSync} from 'node:fs';
import {describe, expect, it} from 'vitest';

import {MESSAGES} from './messages.js';
import {PILLS} from './model.js';

// every t('…') of the page has its German and English text
describe('messages', () => {
    it('covers every key', () => {
        const dir = process.cwd() + '/src/';
        const missing: string[] = [];
        for (const f of readdirSync(dir).filter((f) => f.endsWith('.svelte'))) {
            const src = readFileSync(dir + f, 'utf8');
            for (const m of src.matchAll(/\bt\((?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")/g)) {
                const key = (m[1] ?? m[2]!).replace(/\\'/g, "'");
                const e = MESSAGES[key] as {de?: string; en?: string} | undefined;
                if (!e?.de || !e?.en) missing.push(`${f}: ${key}`);
            }
        }
        for (const label of Object.values(PILLS)) if (!MESSAGES[label]) missing.push('pill: ' + label);
        expect(missing).toEqual([]);
    });
    it('says "the system", never "the box", and "Sie", never "du"', () => {
        for (const [k, v] of Object.entries(MESSAGES)) {
            const e = v as {de: string; en: string};
            expect(`${k} ${e.de} ${e.en}`).not.toMatch(/\bbox\b/i);
            expect(e.de).not.toMatch(/\b(du|dein|deine|dir|dich)\b/i);
        }
    });
    it('keeps every placeholder in the German text', () => {
        for (const [k, v] of Object.entries(MESSAGES)) {
            const e = v as {de: string; en: string};
            for (const m of k.matchAll(/\{(\w+)\}/g)) expect(e.de, k).toContain(`{${m[1]}}`);
        }
    });
});
