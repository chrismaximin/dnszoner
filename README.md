# dnszoner

A terminal editor for DNS zones across providers (AWS Route53 and Namecheap),
built for people with a lot of domains.

Every zone and record is cached in SQLite, so the app opens instantly with
everything loaded and refreshes from the providers in the background.

## Search

The first screen is a fuzzy search over domain names, hosts and values. Terms
are matched independently, so `app devel` finds the `developers` record of
`applepie.io`. The top result is selected, and Enter edits it.

![Fuzzy search across all domains](docs/search.png)

## Edit a record

The value is selected when the editor opens, so typing replaces it. Tab moves
to the type, host and TTL. The old value of each field stays visible while
you edit. Enter saves and asks whether to apply now or save for later. ctrl+d
deletes the record.

![Editing a record](docs/edit.png)

## Bulk find and replace

Mark records with tab (ctrl+a marks every result) and press Enter to change
them all at once. Find is pre-filled when the marked records share a value.
Leave it empty to replace whole values. Matching also works on part of a
value, so replacing `mailhost.net` updates MX records and SPF TXT records
together. The preview shows every record before and after.

![Find and replace across 325 domains](docs/bulk.png)

## Pending changes

Changes saved for later are listed under ctrl+l. Apply them one by one (Enter)
or all at once (A). Each domain's changes go to the provider in a single
call, with several domains in flight per account. If a record changed at the
provider since the last sync, that change is refused and kept pending; the
rest still apply.

![Pending changes](docs/pending.png)

## Install

```sh
GOPRIVATE=github.com/chrismaximin go install github.com/chrismaximin/dnszoner@latest
```

(`GOPRIVATE` is needed while the repository is private; or clone it and run
`go build`.)

Try it without any account using made-up data (kept in a separate database):

```sh
dnszoner --fake
```

## Configure

List your provider accounts in `~/.config/dnszoner/config.json` (see
`dnszoner -h` for the exact path on your system):

```json
{
  "sources": [
    { "name": "AWS prod", "provider": "route53", "profile": "prod" },
    { "name": "AWS staging", "provider": "route53",
      "access_key_id": "AKIA…", "secret_access_key": "…" },
    { "name": "Namecheap", "provider": "namecheap",
      "api_user": "you", "api_key": "…", "client_ip": "203.0.113.7" }
  ]
}
```

- **route53**: uses `profile`, or static keys, or the usual AWS environment
  variables and shared config.
- **namecheap**: needs API access enabled and your IP whitelisted.
  `client_ip` is discovered automatically when omitted. Calls are rate
  limited to stay under Namecheap's API limits.

## Keys

| Where   | Key            | Action                                  |
| ------- | -------------- | --------------------------------------- |
| Search  | type           | filter                                  |
|         | ↑ ↓ / pgup pgdn | move                                   |
|         | enter          | edit the selected record (or the marked ones) |
|         | tab / ctrl+a   | mark the record / mark every result     |
|         | ctrl+l         | pending changes                         |
|         | ctrl+r         | sync from the providers                 |
|         | esc            | clear the search, then the marks, then quit |
| Edit    | type           | replace the selected field              |
|         | ← →            | edit the field in place                 |
|         | tab            | next field                              |
|         | enter          | save (then enter: apply now, l: later)  |
|         | ctrl+d         | delete the record                       |
| Pending | enter / A      | apply the selected change / all of them |
|         | e / x          | edit / discard the selected change      |

## Flags

```
--config PATH   config file (default: the user config dir)
--db PATH       cache database (default: the user cache dir)
--no-sync       don't refresh from the providers on startup
--fake          use made-up sources, domains and records
--fake-reset    with --fake, regenerate the fake data
```

## Screenshots

The images above are generated from `--fake` data by
`scripts/screenshots.sh` (needs tmux, ffmpeg and
[freeze](https://github.com/charmbracelet/freeze)).
