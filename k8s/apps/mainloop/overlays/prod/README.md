# Production Overlay

This overlay contains production-specific configuration.

## Setup

1. **Copy the example personal config:**

   ```bash
   cp personal-config-patch.yaml.example personal-config-patch.yaml
   ```

2. **Edit `personal-config-patch.yaml` with your values:**
   - Replace `example.com` with your actual domains
   - Replace `yourusername` with your GitHub username (for GHCR)
   - Update `FRONTEND_DOMAIN` to match your frontend domain (required for CORS)

3. **The file is gitignored** - your personal config won't be committed to the repo

## What gets configured

- **HTTPRoutes**: Your custom domain names for frontend and API
- **Frontend deployment**:
  - Your GHCR image
  - `ORIGIN` env var for SvelteKit
- **Backend deployment**:
  - Your GHCR image
  - `FRONTEND_DOMAIN` env var for CORS configuration
- **Agent controller deployment**: Your GHCR image

## Deploy

```bash
kubectl apply -k k8s/apps/mainloop/overlays/prod
```

Or use the auto-deploy loop:

```bash
make deploy-loop
```
