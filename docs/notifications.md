# Notifications

Tell orch where to report problems with an application, and it posts to Slack,
Discord or any webhook when a deploy fails, a backup fails or a cron job fails.
Like everything else, notifications live in `orch.pkl`.

```pkl
notifications {
  new Slack { url = "slack-webhook" }
  new Webhook {
    url = "ops-webhook"
    on { "deploy.succeeded"; "deploy.failed"; "replica.unhealthy" }
  }
}
```

`url` names an application secret that holds the webhook URL, so the URL never
appears in the manifest or its history:

```sh
orch secret set shop slack-webhook --from-stdin <<< "https://hooks.slack.com/services/..."
```

## Events

| Event | Sent when |
| --- | --- |
| `deploy.succeeded` | A deploy or rollback finished with every replica running and ready |
| `deploy.failed` | A deploy finished with no replica running, or its pre-deploy hook failed |
| `deploy.rolled_back` | A deploy failed its health gate and orch went back to the previous revision |
| `backup.failed` | A scheduled volume or database backup failed |
| `cron.failed` | A cron run failed or timed out |
| `replica.unhealthy` | A replica that was healthy started failing its readiness checks |

A notification without `on` gets the failures: `deploy.failed`,
`deploy.rolled_back`, `backup.failed` and `cron.failed`. Setting `on` replaces
that list.

## Targets

| Class | Sends |
| --- | --- |
| `Slack` | A Slack [incoming webhook](https://api.slack.com/messaging/webhooks) message |
| `Discord` | A Discord channel webhook message |
| `Webhook` | A JSON document, below, to any `http` or `https` URL |

Slack and Discord get one line, such as
`[production] shop: deploy failed (main@3f2a9c1 by torsten): no desired process is running`.
The cluster name comes first, then the application. Deploys started from
GitHub include the branch, commit and actor.

A `Webhook` is sent:

```json
{
  "event": "backup.failed",
  "cluster": "production",
  "application": "shop",
  "text": "[production] shop: backup of volume main-backup failed: ...",
  "time": "2026-10-20T03:00:12Z",
  "data": { "application": "shop", "volume": "main-backup", "node_id": "node_...", "failed_at": "...", "message": "..." }
}
```

`data` is the underlying event; its fields depend on the event.

## Delivery

The controller sends each event once, in order, and remembers where it got to
across restarts. A request that fails with a network error, `429` or a `5xx` is
retried twice; after that the event is skipped for that target and the reason
is logged by the controller. A missing secret is logged the same way. Events
from before notifications were first configured on a cluster are not sent.

A secret that a notification uses cannot be deleted while the application
references it.
