# orch website

The public product and documentation site is generated from the repository's
Markdown with [Docia](https://docia.xyz/). Dependencies are pinned by
`package.json` and `bun.lock`.

From this directory:

```bash
bun install --frozen-lockfile
bun run check
bun run dev
```

`./build.sh` checks links and navigation, builds production assets into `dist`,
and adds the Sites worker. Generated output is ignored and must not be edited.
