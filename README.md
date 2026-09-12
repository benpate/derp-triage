# Error Triage

Emissary reports its runtime errors to MongoDB through [tools/derp-mongo](../tools/derp-mongo/), which writes one `ErrorLog` record per `derp.Report` call. On a busy server that collection is mostly noise: a remote host that stopped resolving, a peer answering `403`, the same defect recorded four hundred times. Reading it raw is how a real bug stays invisible for a week.

This command turns that collection into a work queue. Every record is classified by where the failure actually originated, stripped of the credentials that derp captures alongside a failed HTTP request, and reduced to a fingerprint so that every occurrence of one defect counts as one item. What is left is short enough to work through: on a log of seven hundred records, thirteen distinct things are broken.

## The loop

Pull one error, investigate it, then say what you decided. `triage fixed` deletes every record of a repaired defect, so the queue drains as the work gets done, and `triage ignore` silences an error that is understood and not worth fixing while leaving its records in place as evidence.

Decisions are kept in a local file rather than in the database, because they have to outlive the records they describe. That file is also what makes a fix verifiable: an error recorded as fixed that happens again is reported as a `regression` instead of arriving as a brand-new discovery.

```shell
triage next                          # one error to work on, and how many remain
triage show <recordID>               # the full, redacted report for one error
triage fixed <recordID> -note "..."  # DELETE every record of it, and remember why
triage ignore <recordID> -note "..." # stop reporting it, keep the records
triage latest                        # every distinct error worth investigating
triage watch                         # one line per new error, as it happens
triage decisions                     # everything already fixed or ignored
triage forget <fingerprint>          # undo a decision, so the error comes back
triage config                        # the settings in use, and where they came from
triage init                          # write a starter configuration file
```

## Configuration

Everything describing *where* to look lives in `triage.json`: which database holds the log, where decisions are recorded, and the default for every flag below. Command-line flags are per-invocation choices only, so nothing that selects a database can be typed by accident.

```json
{
	"connectString": "mongodb://127.0.0.1:27017/?directConnection=true&replicaSet=rs0",
	"database": "Common",
	"stateFile": "/Users/me/Library/Application Support/emissary-triage/decisions.json",
	"scanLimit": 500,
	"includeAll": false,
	"includeDecided": false,
	"includeDuplicates": false
}
```

`triage init` writes a starter `./triage.json` in the working directory, with the connection left empty for you to fill in. It says so on its own output, because no other command runs until you do. The file is searched for in this order, and the first one found wins:

1. the file named by `-config`
2. the file named by the `TRIAGE_CONFIG` environment variable
3. `./triage.json`, where `init` writes
4. `triage.json` in the user configuration directory, shared by every directory on the machine

The database comes from this file and nowhere else. Nothing is inherited from Emissary's own `config.json`, so the only way to change which server triage reads is to edit a configuration file or name another one with `-config`. Keep a second file for each server you work through.

Run `triage config` to see the settings in use and which file they came from. Do that before deleting anything, because `fixed` reports the database it acted on for the same reason.

### Watching a live server

`triage watch` opens a MongoDB change stream rather than polling, so a new error arrives the moment it is written. It prints one compact line per error, which makes it usable as the event source for an agent that troubleshoots errors as they happen.

```shell
triage watch | while read -r line; do echo "$line"; done
```

## What gets hidden, and why

Errors whose root cause is the outbound HTTP client are categorized as `remote-transport` or `peer-response` and left out by default. A DNS failure or somebody else's `502` is not a defect on this side, and burying the real bugs underneath them is the whole problem this command exists to solve. Pass `-all` when you want to see them anyway, or set `includeAll` to make that the default.

Everything else stays visible, deliberately — including an unrecognized failure inside the HTTP client, which is far more likely to be a URL Emissary built wrong than a network event.

## No Warranty

This software is provided as-is, without any warranty of any kind. Use it at your own risk.
