# Base stage with common setup
FROM node:22-alpine AS base

WORKDIR /app

RUN npm install -g pnpm@9.15.9 --fetch-timeout=30000 --fetch-retries=1

# Create non-root user
RUN addgroup -g 1001 -S nodejs && adduser -S -u 1001 -G nodejs nodejs

# Copy root package files
COPY --chown=nodejs:nodejs package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY --chown=nodejs:nodejs scripts/with-timeout.mjs ./scripts/with-timeout.mjs

# Copy workspace packages
COPY --chown=nodejs:nodejs packages ./packages
COPY --chown=nodejs:nodejs frontend ./frontend

# Development stage
FROM base AS dev

RUN node scripts/with-timeout.mjs 120 'pnpm install' -- pnpm install --frozen-lockfile

RUN chown -R nodejs:nodejs /app

WORKDIR /app/frontend

USER nodejs

EXPOSE 3000

CMD ["pnpm", "dev", "--host", "0.0.0.0", "--port", "3000"]

# Build stage for production
FROM base AS builder

RUN node scripts/with-timeout.mjs 120 'pnpm install' -- pnpm install --frozen-lockfile

WORKDIR /app/frontend
ARG VITE_API_URL=https://mainloop-api.olds.network
ENV VITE_API_URL=$VITE_API_URL
RUN pnpm run build

# Production runtime stage
FROM node:22-alpine AS prod

WORKDIR /app

RUN npm install -g pnpm@9.15.9 --fetch-timeout=30000 --fetch-retries=1

# Copy root package files
COPY --from=builder /app/package.json /app/pnpm-lock.yaml /app/pnpm-workspace.yaml /app/.npmrc ./
COPY --from=builder /app/scripts ./scripts

# Copy workspace packages
COPY --from=builder /app/packages ./packages

# Copy frontend
COPY --from=builder /app/frontend ./frontend

RUN node scripts/with-timeout.mjs 120 'pnpm install --prod' -- pnpm install --frozen-lockfile --prod

RUN addgroup -g 1001 -S nodejs && adduser -S nextjs -u 1001

WORKDIR /app/frontend

RUN chown -R nextjs:nodejs /app

USER nextjs

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:3000/ || exit 1

CMD ["node", "build"]
