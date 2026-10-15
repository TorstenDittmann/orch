FROM oven/bun:1.4.0 AS build

WORKDIR /site/website
COPY website/package.json website/bun.lock ./
RUN bun install --frozen-lockfile

COPY docs /site/docs
COPY website/docia.config.ts ./
COPY website/public ./public

ARG SITE_URL
RUN SITE_URL="$SITE_URL" bun run check && SITE_URL="$SITE_URL" bun run build:docs

FROM nginxinc/nginx-unprivileged:stable-alpine

COPY website/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /site/website/dist /usr/share/nginx/html

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/ || exit 1
