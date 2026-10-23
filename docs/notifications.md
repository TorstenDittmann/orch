# Notifications

Tell orch where to report problems, and it posts to Slack, Discord or any
webhook. Problems with an application, such as a failed deploy, backup or cron
job, are set in that application's `orch.pkl`. Problems with the cluster
itself, such as a lost node, are set once for the cluster with
[`orch notify`](#cluster-notifications).

## Application notifications

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

### Events

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

### Targets

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

## Cluster notifications

One command sends cluster problems to Slack, and sends a test message so you
know it works:

```sh
orch notify add slack https://hooks.slack.com/services/...
```

The URL is stored encrypted on the controller. Leave it out to read it from
stdin instead, which keeps it out of your shell history. `discord` and
`webhook` work the same way.

| Event | Sent when |
| --- | --- |
| `node.lost` | A node has had no heartbeat for 3 minutes |
| `node.disk_low` | A node's disk is 90% full; sent again only after it drops below 85% |
| `certificate.expiring` | The controller, cluster CA, a node's agent or a recently used administrator certificate expires within 30 days |
| `backup.stale` | The newest automatic controller backup is more than 2 days old |

Each problem is sent once when it starts, and again only if it clears and
comes back. Agents renew their certificates 90 days ahead, so an agent
certificate reaching the 30-day line means renewal is failing on that node.

`--on` picks the events instead. It can also name any application event,
which is then sent for every application, so one channel can hear about
everything:

```sh
orch notify add slack --name ops --on node.lost,deploy.failed,backup.failed < slack-url
```

```sh
orch notify list               # name, kind and events
orch notify test [NAME]        # send a test message now
orch notify remove NAME
```

Each notification has a name, `slack` by default for `add slack`; adding the
same name again replaces it. Slack and Discord get one line such as
`[production] node web-2 is unreachable; last heartbeat 2026-10-23T14:03:00Z`.
A `Webhook` gets the same JSON document as above, without `application`, and
with `data` holding the node, the certificate or the message.

## Delivery

The controller sends each event once, in order, and remembers where it got to
across restarts. A request that fails with a network error, `429` or a `5xx` is
retried twice; after that the event is skipped for that target and the reason
is logged by the controller. A missing secret is logged the same way. Events
from before notifications were first configured on a cluster are not sent.

A secret that a notification uses cannot be deleted while the application
references it.

Cluster problems are checked once a minute. A node that was already down
while the controller itself was stopped counts as lost only 3 minutes after
the controller starts again, so restarting the controller does not report
every node.
