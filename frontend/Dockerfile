# Build stage
FROM node:22-alpine AS builder

WORKDIR /app

# Copy root package files
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./

# Copy workspace packages
COPY packages ./packages
COPY frontend ./frontend

# Install pnpm and dependencies
RUN npm install -g pnpm@9 && pnpm install --frozen-lockfile

# Build frontend
WORKDIR /app/frontend
ENV USE_NODE_ADAPTER=true
ARG VITE_API_URL=https://mainloop-api.olds.network
ENV VITE_API_URL=$VITE_API_URL
RUN pnpm run build

# Runtime stage
FROM node:22-alpine

WORKDIR /app

# Copy root package files
COPY --from=builder /app/package.json /app/pnpm-lock.yaml /app/pnpm-workspace.yaml ./

# Copy workspace packages
COPY --from=builder /app/packages ./packages

# Copy frontend
COPY --from=builder /app/frontend ./frontend

# Install pnpm and production dependencies
RUN npm install -g pnpm@9 && pnpm install --frozen-lockfile --prod

# Create non-root user
RUN addgroup -g 1001 -S nodejs && adduser -S nextjs -u 1001

WORKDIR /app/frontend

# Change ownership to non-root user
RUN chown -R nextjs:nodejs /app

USER nextjs

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:3000/ || exit 1

CMD ["node", "build"]
