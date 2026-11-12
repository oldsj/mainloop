# Canary

`mainloop canary` runs one cheap synthetic turn through a running Mainloop and posts to ntfy when a
step fails or the reply is slow. The chart runs it as a CronJob (`canary.enabled`, off by
default). Implemented in `server/internal/canary`; tested against a fake API and ntfy only.

## One run

For each `--target name=url[@host]` (in the chart, this instance's own API, named `canary.name`):

| Step | What it does |
| --- | --- |
| `list-sessions` | `GET /sessions`, to find today's canary session |
| `create-session` | first run of the UTC day: creates `Mainloop canary YYYY-MM-DD` with the prompt `ping` |
| `set-model` | right after the create, sets the session's model override (`--model`, default `haiku`) |
| `send-message` | later runs: sends `ping` to today's session; anything but `ok` (queued, steered) fails, since it means the previous canary turn never ended |
| `await-reply` | reads the conversation until an assistant reply follows the ping (`--deadline`, default 4m); fails early if the session ends |
| `archive-old` | after a reply, archives earlier days' canary sessions |

A reply slower than `--slow` (default 90s) is reported as slow. All targets run in parallel; one
ntfy notification covers every target that failed (priority 4, "Mainloop canary failed") or was
only slow (priority 3), naming the target and the step. A passing run posts nothing. The exit code
is non-zero when any target failed, so the Job history shows failures too.

ntfy comes from `MAINLOOP_NTFY_URL`, `MAINLOOP_NTFY_TOPIC` and `MAINLOOP_NTFY_TOKEN` (see
[push notifications](push-notifications.md)); without them the run only logs.

## Cost

Each run is one native turn on the smallest model with a one-word prompt, in a session of its own,
so it never touches the main thread, tasks or the Needs-you inbox. The session is replaced every
day so its context stays small. The first turn of each day runs on the agent's default model if
the model override lands after the turn was sent (the native session normally takes seconds to
start, so the override usually wins).

The default schedule is every 10 minutes: 144 small turns a day. Every 5 minutes halves the time
to detect an outage and doubles the turns. Failing runs alert every run, so an outage pages every
10 minutes until fixed.

## Deployment

The CronJob runs in the instance's namespace with the backend image; the chart adds an ingress
rule letting pods labelled `app: mainloop-canary` reach the API on 8000. Each instance (prod,
mainloop-dev) runs its own canary rather than one reaching across namespaces.
