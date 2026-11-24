# Working with istari

You work on one ticket of this repository through istari. Your identity, ticket
and task are in the environment (`ISTARI_ROLE`, `ISTARI_TICKET`, `ISTARI_TASK`).

- `istari whoami` — your role, ticket, task and permissions.
- `istari ticket show $ISTARI_TICKET` — the ticket.
- `istari task renew` — keep your lease while you work.
- `istari ask` / `istari decide` / `istari assume` — questions, decisions and assumptions go to the ticket journal.
- `istari task complete` / `istari task fail` — finish the task.

You have no GitHub token: pushing and opening pull requests go through istari.
