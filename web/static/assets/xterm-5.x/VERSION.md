# xterm.js vendor

## xterm 5.5.0

- Source: https://www.npmjs.com/package/@xterm/xterm/v/5.5.0
- File: `xterm.js` (UMD build from `@xterm/xterm@5.5.0` npm package `lib/xterm.js`)
- SHA256: `1f991ac3b4b283ebf96e60ae23a00a52765dd3a2e46fa6fdda9f1aab032f7495`

- File: `xterm.css` (from `@xterm/xterm@5.5.0` npm package `css/xterm.css`)
- SHA256: `ba8e6985669488981ccf40c0cefe3aba80722cb6c92de7ad628b0bd717faf2b6`

- Upgraded from 5.3.0 (2026-08-13) to fix DOM renderer character-cell metrics: 5.4.0
  changelog "New default text metrics measure strategy (#4929) ... improve cases where
  characters would be cut off." Symptom: the Claude Code startup banner mascot
  (Unicode Block Elements U+2580-259F) rendered with visible gaps/seams between
  adjacent block glyphs in the web terminal (DOM renderer, no canvas/webgl addon).
  Package also migrated from unscoped `xterm` to the `@xterm` npm scope as of 5.4.0.

## @xterm/addon-fit 0.11.0

- Source: https://www.npmjs.com/package/@xterm/addon-fit/v/0.11.0
- File: `addon-fit.mjs` (ESM build from `@xterm/addon-fit@0.11.0` npm package `lib/addon-fit.mjs`)
- SHA256: `2d87e1bddc73be9111de8beee5370c3bb7aac9c94e18e6f245f02ca741ef1769`
