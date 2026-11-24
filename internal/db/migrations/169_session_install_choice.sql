-- install_choice: the user's answer to the install offer a client request
-- (client_requests) put in the client's own dialog for this conversation:
-- "pending" while the dialog is open, then "setup" (they'll run the steps
-- themselves), "later" ("Not now"), or what the SDK saw instead ("decline",
-- "cancel", "timeout", "unsupported"). A later subscribing result reads it
-- (harnesses.CodexElicitAskedNote). install_choice_at is when it was set.
-- NULL: no dialog was shown. New nullable columns on one table: no table
-- rewrite, and the previous revision ignores them.
ALTER TABLE sessions ADD COLUMN install_choice text, ADD COLUMN install_choice_at timestamptz;
