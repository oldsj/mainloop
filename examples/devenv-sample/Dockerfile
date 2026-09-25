FROM node:22-bookworm-slim
WORKDIR /workspace
COPY package.json ./
RUN npm install --omit=dev
COPY server.mjs ./
RUN groupadd --system app \
    && useradd --system --gid app --create-home --home-dir /home/app app \
    && chown -R app:app /workspace
USER app:app
ENV PORT=3000 PGHOST=postgres PGPORT=5432 PGUSER=postgres PGPASSWORD=dev-only PGDATABASE=workspace
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD ["node", "-e", "fetch('http://127.0.0.1:3000/').then((response) => process.exit(response.ok ? 0 : 1)).catch(() => process.exit(1))"]
CMD ["npm", "start"]
