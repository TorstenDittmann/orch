FROM oven/bun:1.4.0

WORKDIR /site/website
COPY website/package.json website/bun.lock ./
RUN bun install --frozen-lockfile

COPY docs /site/docs
COPY website/tsconfig.json website/vite.config.ts website/serve.ts ./
COPY website/src ./src
COPY website/static ./static

ARG SITE_URL
RUN SITE_URL="$SITE_URL" bun run check && SITE_URL="$SITE_URL" bun run build:site

USER bun
EXPOSE 8080
CMD ["bun", "run", "start"]
