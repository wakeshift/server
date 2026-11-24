-- Invites: sign-up is open to anyone with an invite code (besides the
-- allow list). Each user gets a code to share, good for a few sign-ups; an
-- operator's code (no created_by, added by hand) can have any number. Not
-- tenant-scoped, like users.
CREATE TABLE invites (
    code       text        PRIMARY KEY,
    created_by uuid        UNIQUE REFERENCES users(id) ON DELETE CASCADE, -- one per user
    uses_left  int         NOT NULL CHECK (uses_left >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Who invited whom (the code's owner, or NULL for an operator's code or the
-- allow list).
ALTER TABLE users ADD COLUMN invited_by uuid REFERENCES users(id) ON DELETE SET NULL;
