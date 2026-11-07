# The agent sandbox

`hyperbutler sandbox` runs a coding agent, or any command, in a Linux VM of its own. The
agent works on a copy of your project. Nothing it does reaches your files until you read
the changes and apply them, and the network it can reach is decided by a mode you choose.

The design and the threats it answers are in [ADR 0009](adr/0009-agent-sandbox.md).

You need the same machine as for the Docker engine (Windows 11, x64, virtualization on).
The Docker VM does not have to be running, and a session does not use it.

## A session

```powershell
hyperbutler sandbox run --name work C:\src\myapp
```

This copies the project into a new session and opens a shell in `/workspace`. Install and
start your agent there; it stays on the session's disk:

```bash
npm i -g @openai/codex
codex
```

To pass an API key from your environment, name the variable. The value travels only to
that terminal and never appears on a command line:

```powershell
hyperbutler sandbox run --name work --env OPENAI_API_KEY C:\src\myapp codex
```

Ctrl+] leaves the terminal and the session goes on; what the agent runs keeps running.

```powershell
hyperbutler sandbox attach work   # back to the first terminal
hyperbutler sandbox shell work    # another terminal
hyperbutler sandbox ls
```

## Bringing the work out

```powershell
hyperbutler sandbox diff work          # every change, compared with your files now
hyperbutler sandbox diff work --stat   # just the list
hyperbutler sandbox apply work --all
hyperbutler sandbox apply work --path src/app.js --path README.md
hyperbutler sandbox commits work       # the agent's commits, into branch sandbox/work
```

`apply` writes exactly the bytes `diff` read; it does not ask the session again, so you must
run `diff` first. `--all` takes only what `diff` showed whole: after `--stat`, or for a change
too long to show, name the file with `--path`. What the review says:

- **RISK**: a change that can run code or point a tool at a server later: a `Makefile`, a
  CI workflow, a package manifest, editor tasks, a coding agent's settings file, a new
  executable. `--all` leaves these out. Read them, then name each with `--path`.
- **CONFLICT**: you changed that file on this computer since the copy. It is not
  overwritten.
- **NOT APPLICABLE**: a name Windows could misread, a symbolic link, a file over 64 MiB, or
  anything inside `.git`. It is listed and never written.

Names and text from the session are shown escaped, so a file name cannot move the cursor,
reverse the text or pretend to be another file.

`commits` fetches into a branch and checks nothing out. The branch name is yours, no hook
of your repository runs, and git checks the objects it receives. A branch that has moved
on is an error, not an overwrite. This works when the folder you gave is the top of its
repository.

## What goes into a session

In a git repository: the tracked files and the untracked ones git does not ignore. Elsewhere:
every file. Left out, and listed when the session starts:

- files named like secrets (`.env*`, `*.pem`, `*.key`, `id_*`, `.npmrc`, `.netrc`, kubeconfigs
  and so on) and files holding a known token format or a private key;
- `.git` itself. History comes in as a bundle of the branches and tags, with a generated
  config: no reflog, stash, remotes or credentials. A key you removed in a later commit is
  still in the history a bundle carries.

`--include PATH` brings a listed file in. The check is a net, not a guarantee: read the list.

## The network

| Mode | The session can reach | Does it stop your code being sent somewhere? |
|---|---|---|
| `dev` (default) | package registries, git hosts, container registries and model providers' API hosts, by exact name, and what you add | It narrows it. Several of those hosts accept uploads from anyone |
| `locked` | only what you add | Yes, as far as the hosts you add do not accept uploads |
| `open` | what the Docker VM can reach, your LAN and VPN included | No |

```powershell
hyperbutler sandbox run --egress locked --allow api.openai.com C:\src\myapp
hyperbutler sandbox allow work pypi.org github.com:22
hyperbutler sandbox blocked work   # what it asked for and did not get
```

`--allow` takes `host`, `host:port` (a port other than 443 and 80), `*.host` (capped in how
many names it may resolve) and `lan:host` (a name that may answer with a private address).
Use the ASCII form of a name (punycode).

`*.host` allows 64 distinct names per session; repeated queries are not bounded. A wildcard on a
public suffix (`*.co.uk`, `*.github.io`) is refused. One allowed name the agent can influence opens
ports 443 and 80 to any public address it answers with, and a server with a default virtual host
does not care about the server name, so allow only names whose answers you trust.

In `dev` and `locked`:

- A name outside the list never reaches your DNS server, so a lookup cannot carry data out.
- A connection goes only to an address an allowed name just answered with. On port 443 the
  TLS server name must be that name, and a connection with no server name or an Encrypted
  ClientHello is refused. On port 80 (not in `locked`) the `Host` header must match.
- UDP other than DNS is not forwarded; QUIC falls back to TCP.
- A session never reaches your computer's own services, your LAN (unless `lan:`), another
  session or the Docker VM.

Known limits: traffic inside TLS is not seen, so a CDN that serves several sites on one name
(domain fronting) and DNS over HTTPS to an allowed host cannot be told apart; on port 80 only
the first request of a connection is checked; a port you name is checked by address only.

## Ending a session

```powershell
hyperbutler sandbox stop work     # a named session keeps its disk; start it again later
hyperbutler sandbox start work
hyperbutler sandbox rm work       # disk and record gone
```

A session without `--name` is deleted when you stop it. Both that and `rm` refuse while there
are changes nobody applied; `--discard` overrides that.

`hyperbutler uninstall` removes every session too; `--keep-data` keeps their disks and records.
Moving the data folder (`config set data`) is refused while a session's disk is in it.

At most four sessions exist at once, stopped ones included, and the broker refuses a start
that would take your running VMs past 75% of physical memory. Each session takes 2 GiB by
default (`--memory`) and its disk grows as it is used, up to 64 GiB.

## What the session can do to you

The agent runs as an ordinary user in its own VM, with its own kernel, and sees none of your disk. What crosses back is what you apply,
what you fetch and what it prints on your terminal. Its output reaches your terminal raw, as
over ssh, so a hostile program can use whatever your terminal emulator offers to programs it
runs. Windows Terminal, for example, lets a program set the clipboard: be careful what you paste
after leaving a session you do not trust.

Files you `--include`, variables you `--env` and everything in the project are the agent's to
read, and with an open network, to send.
