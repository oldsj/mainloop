# Mainloop dev environment sample

This small fixture demonstrates the shape of a branch workspace: one Node app, one sibling
Postgres service, an HTTP preview port, a WebSocket echo endpoint, and a persistent counter.

The app serves one page at `/`, echoes WebSocket messages at `/ws`, and reads or increments a
Postgres row through `/api/counter`. `mainloop.yaml` declares the app image, service, preview
port, actor template, and 30-minute idle timeout. Build the image from this directory with
`docker build -t mainloop-devenv-sample:latest .` in an environment with Docker available.

The image declares a real non-root `app` user and checks the app's `/` HTTP route with a
container health check. The current Substrate actor path overrides the image's `USER` and runs
actors as UID 0; that upstream runtime gap remains. The sample does not claim to validate
non-root actor execution or live multi-container connectivity. Sample credentials are
fixture-only; use secret-backed values for real projects.
