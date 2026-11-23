# Fliessband UI

React, Vite, Tailwind CSS v4 and [shadcn/ui](https://ui.shadcn.com) (Radix, Nova preset).
The build goes to `../static`, which the Go binary embeds, so running Fliessband needs no Node.

```bash
npm ci
npm run dev     # http://localhost:5173, proxies /api to fliessband on :7070
npm run build   # or `make ui` from the fliessband directory
```

Add components with `npx shadcn@latest add <name>`.
