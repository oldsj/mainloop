# Build from the repository root: docker build -f web/Dockerfile .
#
#   --build-arg VITE_API_URL=https://api.example.test   API base URL baked into the bundle
#                                                       (default /api, which needs a proxy)

FROM node:24-alpine AS web
WORKDIR /app
RUN npm install -g pnpm@9.15.9 --fetch-timeout=30000 --fetch-retries=1

# Install from the lockfile. Every workspace manifest is needed for --frozen-lockfile to match.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY packages/ui/package.json ./packages/ui/package.json
COPY frontend/package.json ./frontend/package.json
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
