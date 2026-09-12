/*
Package main implements the "triage" command, which turns Emissary's MongoDB
error log into a work queue.

The ErrorLog collection that tools/derp-mongo writes is noisy: most records
are a remote server timing out or answering 403, and one real defect appears
hundreds of times.  Triage classifies each record by where it actually came
from, strips the credentials that derp captures alongside a failed HTTP
request, and reduces every occurrence of one defect to a single fingerprint.

Fixing a defect means deleting its records, so the log drains as the work is
done.  Decisions are kept in a local file, which outlives the records and
reports a fixed error that happens again as a regression.

See README.md for the commands, and AGENTS.md for the rules behind them.
*/
package main
