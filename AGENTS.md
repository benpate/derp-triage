# triage — Notes for AI Agents

See [README.md](README.md) for what this command does. These are the rules that are not visible in the code.

## Changing how a fingerprint is computed invalidates every decision ever recorded

[Fingerprint](report.go) and [NormalizeMessage](report.go) together define the identity of an error, and that identity is **written to disk** in the decision file. Every `fixed` and `ignore` is stored under the hash those two functions produced at the time.

So editing either one — a new placeholder, another regexp, a different separator, a change to which fields go into the seed — silently orphans every decision on every machine that has one. Errors that were fixed months ago come back as fresh work, and nothing reports that anything happened, because a fingerprint that matches nothing looks exactly like a fingerprint that was never recorded.

If a change is genuinely needed, it is a migration: bump a version into the seed deliberately, and expect operators to re-decide. Do not treat it as a refactor.

## `NormalizeMessage` is single-pass, and is not idempotent

Each replacement rewrites the word boundaries around the text it replaced, which exposes matches the previous pass could not reach. `A000000000000a.aaaaa` normalizes to `A<n>a.aaaaa`, and normalizing *that* gives `A<n><host>`, because `<n>` put a boundary in front of `a.aaaaa` where none existed.

This was chased far enough to establish that it cannot be fixed cheaply. Word-safe placeholders (`xnum0`) move the problem rather than solving it — they become valid URL schemes and domain suffixes in their own right, so `0000://0` normalizes to a URL on the second pass. Escaping every placeholder against every pattern is the only complete fix, and it is not worth it.

It does not need to be idempotent. `Fingerprint` is the only caller, and it normalizes a raw message exactly once. The property that actually matters is determinism, which is what the fuzz target asserts. Do not add a second call, and do not "fix" the non-idempotence.

## Redaction happens by NAME, before a value is ever read

Most records in a live log embed a signed HTTP `Signature` header, and some embed a session `Cookie`. [Redact](redact.go) discards those by matching the map key, never by inspecting the value — a value-based rule would have to decide what a secret looks like, and it would be wrong.

Two consequences. The `Report` is the only form of a record that may be printed, logged, or handed to a model; the raw `Record` must not leave this package. And `toObject` / `toSlice` in [record.go](record.go) exist because the BSON decoder produces `bson.M` and `bson.A`, which are **defined types** — a plain `map[string]any` type assertion fails against them, and every nested header would have passed through unredacted with no error anywhere.

## Where to look is configuration, never a command-line parameter

`-uri`, `-db`, and `-state` used to be flags and deliberately are not any more. Every setting that selects a database or a decision file lives in `triage.json`, because `fixed` deletes records and a mistyped flag is the one way to delete them from the wrong server. The flags that remain describe a single invocation: what to note, whether to scan further, whether this is a dry run.

Two rules follow from that, and both matter:

A path given to `-config` **must exist**. [readConfig](config.go) does not fall back to the search path when a named file is missing, because falling back is precisely how a command aimed at production would quietly run against a development database instead.

Only flags the caller actually typed override the file. [loadConfig](main.go) uses `flags.Visit`, which reports only the flags that were set — every boolean's zero value is also a legitimate configured value, so registering a default of `false` and copying it unconditionally would silently undo a configured `true`. The configuration is re-validated afterwards, since `-n 0` can turn a valid file into an invalid run.

`triage.json` is in the repository's `.gitignore` alongside Emissary's own `config.json`, and `triage init` writes it `0600`. A connection string carries a password.

`init` writes to the **working directory**, not a machine-wide location, so each checkout and each deployment carries the settings for its own database. It writes an empty connection and says so in a `warning` on its own output, because the file is unusable by every other command until somebody fills it in, and `init` refuses to overwrite it on the second attempt. For the same reason [Config.Validate](config.go) names the file to edit rather than telling somebody to run `init` again.

The connection is **never** inherited from anywhere else. An earlier version borrowed `activityPubCache` out of Emissary's own `config.json` whenever triage named no database of its own, which meant a command typed in a server's working directory silently acquired a database nobody had written down. Since `fixed` deletes records, the database in play must be traceable to one file that somebody edited on purpose.

## The decision file is not a cache

[DefaultStateFile](config.go) puts decisions under `os.UserConfigDir()`, never `os.UserCacheDir()`. They are the durable record of what has been fixed — the records they describe are deleted, and the log is purged weekly anyway — so a directory the operating system is free to clear would silently resurrect finished work and re-open every closed error.

## Only a named target can ever be deleted

`fixed` is the one command that writes to the database. It resolves exactly one record ID or fingerprint, collects the matching records, and deletes them by `_id`. [Store.Delete](store.go) returns early on an empty list specifically so that an empty `$in` can never become a filter that matches the whole collection.

There is deliberately no command that empties the log, and no way to delete by category, date, or pattern. `-dry-run` reports what a call would remove without removing it.

## `Analyze` and `Fingerprint` must never disagree

Both read [Record.identity](record.go), and that is the point. `Fingerprint()` is the cheap path used while scanning thousands of candidate records; `Analyze()` is the full path that builds a report. If they computed identity separately, a drift between them would make `fixed` delete the wrong records — or silently find none, report `matched: 0`, and record the decision anyway.

`Record.Criteria()` narrows that scan to records sharing a status code and root location. That is sound **only** because both go into the fingerprint seed verbatim, without normalization. Normalizing either one would make the pre-filter start excluding records that do match.

## A report's date is only accurate to the second

`Report.CreateDate` is RFC3339, which truncates. The regression check in [filter.go](filter.go) therefore compares against `DecidedAt.Truncate(time.Second)` and treats a tie as a recurrence, because missing a regression (a defect you believe is fixed is still live) is worse than reporting one twice.

## Watch needs a replica set; the log is purged weekly

`triage watch` opens a change stream, which MongoDB only offers on a replica set. Against a standalone server it fails at open with a message saying so, and `latest` is the fallback.

Separately, `consumer.PurgeErrors` deletes anything older than seven days. Nothing in the log is a durable record of anything, which is why the decision file is kept locally and why a high-water mark on `_id` would silently skip: the floor moves out from under it.

## Word-boundary matching, not `strings.Contains`

[containsPhrase](filter.go) requires non-alphanumeric edges around a match, so `eof` does not fire inside `neofetch`. A short token matched with `Contains` classifies a real defect as a network hiccup, and it is then filtered out of every default view and never investigated. This is the same trap that made [benpate/sniff](https://github.com/benpate/sniff) read `Microsoft` as ChromeOS.

## Tests need a local MongoDB, and each one gets its own database

[integration_test.go](integration_test.go) covers the Mongo adapter and the command runners, which are otherwise untestable and were at 0% before it existed. It skips under `-short`, and skips (rather than fails) when MongoDB is unreachable.

Every test creates a database named for a fresh ObjectID, writes a `triage.json` pointing at it, and drops the database in cleanup — so a failing test can never reach the real error log. Local connect strings need `?directConnection=true`; see the root [AGENTS.md](../AGENTS.md).

Any test that resolves configuration without an explicit path **must** call `isolate`, which points `HOME` and the working directory at empty temporary directories. The search path ends at the user configuration directory, so without it a test would read whatever real `triage.json` is on the machine running it, and pass or fail according to someone's local settings.
