# Working with istari

This repository is run by istari: work goes through tickets, and a ticket
gets into work only when it meets the ticket standard of this repository
(`.istari/ticket-standard.md` and `.istari/ticket-standard.toml`).

## Writing tickets

- `istari ticket check --file ticket.md` shows how a draft deviates from the standard, without changing anything.
- `istari ticket create --file ticket.md` creates the ticket; `--file plan.yaml` creates a set of linked tickets.

## Working on a ticket

When istari starts you as an agent, your identity is in the environment:
`ISTARI_PROFILE`, `ISTARI_ROLE`, `ISTARI_TICKET`, `ISTARI_TASK` and
`ISTARI_TOKEN`, the run token for `istari` commands.

- `istari whoami` — your role, ticket, task and permissions.
- `istari ticket show $ISTARI_TICKET` — the ticket.
- `istari ask` / `istari decide` / `istari assume` — questions, decisions and assumptions go to the ticket journal.
- `istari checkpoint` — record progress; istari runs the checks itself.

You have no GitHub token: pushing and opening pull requests go through istari.
