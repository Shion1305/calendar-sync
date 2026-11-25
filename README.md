# calendar-sync

One-shot Google Calendar synchronizer for preserving connpass participation
history. Source events are copied or updated; missing/canceled source events
never delete destination events. The subscription remains the source for new
events, so this cannot recover events that vanished before the first sync.

## Matching and archival compatibility

The synchronizer recognizes `connpass_event_id` and `connpass_sync_key` in
restored event descriptions, including Google Calendar's HTML formatting.
It also recognizes the final connpass event URL, an unambiguous event URL,
legacy `sourceEventId` private properties, and identical titles/start/end times.
It adds private `connpass_event_id`, `connpass_sync_key`, `sourceEventId`, and
`sourceCalendarId` properties when it adopts an existing event. Identifiers
are added to descriptions of new connpass copies as well.

Restored descriptions containing `connpass_import_batch` are preserved,
including their source links, excerpts and batch identifiers. Other copies
receive source title, description, location, start and end changes. Existing
reminders, guests and unrelated properties are not replaced. New copies have
no guests or reminders; writes request no notification emails.

Both calendars are fully paginated. An ambiguous match or a failed lookup
stops the run before writing. Recurring sources are explicitly unsupported.
New copies use deterministic Google event IDs so an interrupted/concurrent
insert cannot create a second copy. No destination deletion is implemented.

## Authorization and use

1. Copy `config.sample.yaml` to `config.yaml` and set the calendar IDs.
2. Put the Google OAuth client configuration in `credentials.json`.
3. Run `go run . --authorize`, open the localhost URL, and authorize Google
   Calendar access. This saves `token.json` and exits without syncing.
4. Inspect `go run . --dry-run`, then run `go run .` to synchronize.

The OAuth redirect URI must be `http://localhost:1853/setup/auth/callback`
(or the same localhost callback configured in `config.yaml` and Google).
The browser callback listens only on loopback. Tokens are written with mode
0600. Never commit the config or OAuth files.

Normal runs are noninteractive and require an existing refresh token. They
refresh access tokens in memory; credentials, config and token files can all
be read-only Secret mounts. Google may revoke the refresh token; an
`invalid_grant` error requires local reauthorization and replacement of the
stored token. No PVC is needed.

Flags: `--config`, `--credentials`, `--token`, `--dry-run`, `--since`
(RFC3339 source update filter), `--timeout` (default 8 minutes), and
`--authorize`. The default sync lists all current source events. Errors exit
nonzero so CronJobs report failure. Logs show IDs/counts, not event descriptions
or credentials.

## Container and publication

The image runs as UID/GID 65532 and supports Linux amd64 and arm64:

```sh
docker run --rm \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  -v "$PWD/credentials.json:/secrets/credentials.json:ro" \
  -v "$PWD/token.json:/secrets/token.json:ro" \
  harbor.i.shion1305.com/shion1305/calendar-sync:latest \
  --config /config/config.yaml --credentials /secrets/credentials.json \
  --token /secrets/token.json --dry-run
```

CI tests every push/PR. Repository branch pushes and manual runs publish
commit-SHA tags to Harbor using the shared GitOps build action and GitHub
OIDC/Vault authentication, then sign the image. PR runs only validate builds.
After signing, CI also tags the current default branch image as `latest`.
The calendar-sync CronJob in `Shion1305/k8s-GitOps` pulls that tag for each run.

## Validation

```sh
go test -race ./...
go vet ./...
```
