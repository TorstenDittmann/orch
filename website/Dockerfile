FROM oven/bun:1.4.0

WORKDIR /site/website
COPY website/package.json website/bun.lock ./
RUN bun install --frozen-lockfile

COPY docs /site/docs
COPY website/docia.config.ts ./
COPY website/public ./public

ARG SITE_URL
RUN SITE_URL="$SITE_URL" bun run check && SITE_URL="$SITE_URL" bun run build:docs

USER bun
EXPOSE 8080
CMD ["bun", "run", "start"]
