# Build from the repository root: docker build -f web/Dockerfile .
#
#   --build-arg VITE_API_URL=https://api.example.test   API base URL baked into the bundle
#                                                       (default /api, which needs a proxy)

# dev (docker build --target dev -f web/Dockerfile .): the Vite dev server with HMR. The source
# is baked in so the pod starts before any sync; DevSpace then syncs web/ over it. Runs as uid
# 1001 like the prod image, so the synced files and Vite's caches are writable. The prod image
# below stays the last stage and so the default target.
FROM node:24-alpine AS dev
RUN npm install -g pnpm@9.15.9 --fetch-timeout=30000 --fetch-retries=1 \
    && adduser -D -u 1001 app \
    && mkdir /app \
    && chown 1001:1001 /app
USER 1001:1001
WORKDIR /app
COPY --chown=1001:1001 package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY --chown=1001:1001 web/package.json ./web/package.json
RUN pnpm install --frozen-lockfile --filter web...
COPY --chown=1001:1001 web ./web
WORKDIR /app/web
# vite.config.ts reads the host and port; MAINLOOP_API_PROXY and the allowed host come from the
# deployment.
ENV VITE_HOST=0.0.0.0 \
    VITE_PORT=3000
EXPOSE 3000
CMD ["node_modules/.bin/vite"]

FROM node:24-alpine AS web
WORKDIR /app
RUN npm install -g pnpm@9.15.9 --fetch-timeout=30000 --fetch-retries=1

# Install from the lockfile. Its importers are the root and web/, so both manifests are needed for
# --frozen-lockfile to match.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY web/package.json ./web/package.json
RUN pnpm install --frozen-lockfile --filter web...

COPY web ./web
WORKDIR /app/web
ARG VITE_API_URL=/api
ENV VITE_API_URL=$VITE_API_URL
RUN pnpm run build

FROM golang:1.27-alpine AS serve
WORKDIR /src
COPY web/serve/go.mod ./
COPY web/serve/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /serve .

FROM gcr.io/distroless/static-debian12:nonroot
ARG VITE_API_URL=/api
ENV VITE_API_URL=$VITE_API_URL \
    WEB_DIR=/app/dist \
    PORT=3000
COPY --from=serve /serve /app/serve
COPY --from=web /app/web/dist /app/dist
USER 1001:1001
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/app/serve", "-healthcheck"]
ENTRYPOINT ["/app/serve"]
