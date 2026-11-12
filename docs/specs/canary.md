# Canary

`mainloop canary` runs one cheap synthetic turn through a running Mainloop and posts to ntfy when a
step fails or the reply is slow. The chart runs it as a CronJob (`canary.enabled`, off by
default). Implemented in `server/internal/canary`; tested against a fake API and ntfy only.

## One run

The canary keeps **one long-lived hidden session** per instance and pings it, so each run is a warm
wake of an idle session: the path the owner's own sessions take, and the latency it measures.

For each `--target name=url[@host]` (in the chart, this instance's own API, named `canary.name`):

| Step             | What it does                                                                                                                                                                                                                                                         |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `list-sessions`  | `GET /sessions?hidden=true`; the newest canary session that has not ended is the one to ping                                                                                                                                                                         |
| `create-session` | only when none is usable (first run, or the last one broke): creates a hidden standalone session `Mainloop canary YYYY-MM-DD HH:MM` with the prompt `ping` and the model (`--model`, default `claude-haiku-5-5`) set at create. This run is a cold start and says so |
| `send-message`   | otherwise sends `ping`; anything but `ok` (queued, steered) means an earlier canary turn never ended: it is stopped and the run fails                                                                                                                                |
| `await-reply`    | reads the conversation until an assistant reply follows the ping (`--deadline`, default 4m); on a timeout the turn is stopped (`stop-turn`) so nothing is left running. A model kagent rejects fails here                                                            |
| `cleanup`        | cancels and archives canary sessions that ended or were superseded                                                                                                                                                                                                   |

The session is hidden: `GET /sessions` (and the session list in the attention snapshot) leaves it
out unless `hidden=true` is asked for; it is still readable by id. A standalone session has no
coordination grant, so it never reports to the main thread, and it starts no task. Hiding does not
touch Needs you: a permission request from the canary would still show there, but a one-word
answer uses no tools, and a turn that hangs is stopped long before it could count as stalled.

A reply slower than `--slow` (default 90s) is reported as slow. All targets run in parallel; one
ntfy notification covers every target that failed (priority 4, "Mainloop canary failed") or was
only slow (priority 3), naming the target and the step. A passing run posts nothing. The exit code
is non-zero when any target failed, so the Job history shows failures too.

ntfy comes from `MAINLOOP_NTFY_URL`, `MAINLOOP_NTFY_TOPIC` and `MAINLOOP_NTFY_TOKEN` (see
[push notifications](push-notifications.md)); without them the run only logs.

## Cost

Each run is one native turn on the smallest model with a one-word prompt. The session's context
grows by a few tokens per run; native compaction handles it like any other session. It never
touches the main thread, tasks or the Needs-you inbox.

Standalone sessions run on the claude profile's `agent` Agent (`claude-workspace` in production,
the Kata microVM pool): the canary holds one workspace there while its turn runs and until the
workspace goes idle and is suspended, and each run wakes it. Every run reuses the same session, so
it never adds a second workspace or a cold create.

The default schedule is every 10 minutes: 144 small turns a day. Every 5 minutes halves the time
to detect an outage and doubles the turns. Failing runs alert every run, so an outage pages every
10 minutes until fixed.

## Deployment

The CronJob runs in the instance's namespace with the backend image; the chart adds an ingress
rule letting pods labelled `app: mainloop-canary` reach the API on 8000. Each instance (prod,
mainloop-dev) runs its own canary rather than one reaching across namespaces.
